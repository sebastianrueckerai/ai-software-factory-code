package factoryd

import (
	"cmp"
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
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
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
	tasks           map[string]Task
	sessions        map[string]Session
	started         map[string]bool
	finished        map[string]time.Time
	settled         map[string]bool
	intakeEvents    []IntakeEvent
	confirmedCloses map[[2]any]bool
	// failIntakeEvent is InsertIntakeEvent's error, set to test the writers
	failIntakeEvent error
	events          []Event // newest last, as NewestTaskEvents reads them
	deliveries      map[string]bool
	pruned          []time.Duration
	approvals       []ApprovalRequest
	gates           []string // "task gate since", in order
	projects        []Project
	alertsMu        sync.Mutex
	alerts          []Alert
	rolls           []RollRequest
	chain           []fakeChainRun
	chainNow        func() time.Time // stamps the fake's chain writes; zero where nil
	unsettledReads  int              // the UnsettledChainCommits calls served
	liveSessions    map[string]LiveSession
	pauses          map[string]QueuePause
	// monthSpend is MonthSpend's answer, capRefusals CapRefusal's by task,
	// budgetRefused BudgetRefused's by task; DayFree answers the day's cap
	// less daySpent
	monthSpend    MonthSpend
	capRefusals   map[string]CapRefusal
	budgetRefused map[string]bool
	daySpent      float64
	// providerRefusals are ProviderRefusal's answers by task
	providerRefusals map[string]string
	answered         map[string]bool // ModelAnswered's answer by task
	// checkinCalls and checkinSpent are CheckinCalls' and CheckinSpent's
	// answers by task
	checkinCalls map[string]int
	checkinSpent map[string]float64
	// held are Free's outstanding holds by task (Held, Holds, Expires);
	// capsFree is Free's free day, month and project headroom, all
	// unbounded when nil; openings are Opening's answers by ref
	held      map[string]policy.TaskFree
	capsFree  *policy.Free
	openings  map[string]policy.Opening
	overHolds map[string]OverHoldCall
	// createFails fails that many CreateTask calls
	createFails int
	// createCalls counts every CreateTask call, failed or not
	createCalls      int
	deps             map[string][]string // task → the tasks it depends on
	plans            map[string][]PlanRevision
	questions        map[string][]PlanQuestion     // by "task revision"
	kinds            map[string][]string           // each question's kind beside it, by "task revision"
	catches          map[string][]string           // the recording rule's ledger, by "task revision kind finder"
	criteriaVerdicts map[string][]CriterionVerdict // rows by task, every source's
	holdouts         map[string][]Holdout          // the hold-out rows, idx order, by task
	queue            []queueEntry                  // the merge queues, oldest first
	holds            map[string]RollHold
	// routineStates and routineRuns (oldest first) are keyed by
	// project + "/" + routine
	routineStates map[string]RoutineState
	routineRuns   map[string][]RoutineRun
	routineCursor int64
}

func (s *fakeStore) AddTaskDeps(ctx context.Context, id string, dependsOn []string) error {
	if s.deps == nil {
		s.deps = map[string][]string{}
	}
	for _, d := range dependsOn {
		if d == id {
			return fmt.Errorf("task %s depends on itself", id)
		}
		if !slices.Contains(s.deps[id], d) {
			s.deps[id] = append(s.deps[id], d)
		}
	}
	return nil
}

func (s *fakeStore) OpenDependencies(ctx context.Context, id string) ([]Dependency, error) {
	var out []Dependency
	for _, d := range s.deps[id] {
		if t := s.tasks[d]; t.Status != "done" {
			out = append(out, Dependency{TaskID: d, Issue: t.IssueNumber, Key: t.PlanKey, Status: t.Status})
		}
	}
	slices.SortFunc(out, func(a, b Dependency) int { return int(a.Issue - b.Issue) })
	return out, nil
}

func (s *fakeStore) WaitingDependents(ctx context.Context, id string) ([]string, error) {
	var out []string
	for task, deps := range s.deps {
		if slices.Contains(deps, id) && s.tasks[task].Status == "waiting-deps" {
			out = append(out, task)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (s *fakeStore) ReplaceFailedCard(ctx context.Context, id string, t Task) (bool, error) {
	old := s.tasks[id]
	if old.Status != "failed" || old.PlanKey == "" || old.PlanKey != t.PlanKey {
		return false, nil
	}
	old.PlanKey = ""
	s.tasks[id] = old
	if err := s.CreateTask(ctx, t); err != nil {
		return false, err
	}
	if err := s.AddTaskDeps(ctx, t.ID, s.deps[id]); err != nil {
		return false, err
	}
	for _, deps := range s.deps {
		for i, d := range deps {
			if d == id {
				deps[i] = t.ID
			}
		}
	}
	return true, nil
}

func newFakeStore() *fakeStore {
	return &fakeStore{tasks: map[string]Task{}, sessions: map[string]Session{},
		started: map[string]bool{}, finished: map[string]time.Time{}, settled: map[string]bool{},
		routineStates: map[string]RoutineState{}, routineRuns: map[string][]RoutineRun{}}
}

func (s *fakeStore) CreateTask(ctx context.Context, t Task) error {
	s.createCalls++
	if s.createFails > 0 {
		s.createFails--
		return errors.New("insert failed")
	}
	if t.Status == "merged" && t.FinishedAt == nil {
		panic("fixture " + t.ID + ": merged without finished_at")
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
	// the insert stamps a task already ended with its filing, as the
	// database insert does (roadmap 271)
	if Final(t.Status) && t.FinishedAt == nil {
		t.FinishedAt = &t.CreatedAt
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

func (s *fakeStore) AddPlanRevision(ctx context.Context, id string, plan json.RawMessage, sha string) (int, error) {
	if s.plans == nil {
		s.plans = map[string][]PlanRevision{}
	}
	revs := s.plans[id]
	if n := len(revs); n > 0 && revs[n-1].SHA256 == sha {
		return n, nil
	}
	s.plans[id] = append(revs, PlanRevision{TaskID: id, Revision: len(revs) + 1, Plan: plan, SHA256: sha, CreatedAt: time.Now()})
	return len(revs) + 1, nil
}

func (s *fakeStore) PlanRevision(ctx context.Context, id string, n int) (PlanRevision, bool, error) {
	revs := s.plans[id]
	if n == 0 {
		n = len(revs)
	}
	if n < 1 || n > len(revs) {
		return PlanRevision{}, false, nil
	}
	return revs[n-1], true, nil
}

func (s *fakeStore) CreatePlanCard(ctx context.Context, t Task) (Task, error) {
	for _, have := range s.tasks {
		if have.Parent == t.Parent && have.Source == SourcePlan && have.PlanKey == t.PlanKey {
			return have, nil
		}
	}
	if err := s.CreateTask(ctx, t); err != nil {
		return Task{}, err
	}
	return s.tasks[t.ID], nil
}

func (s *fakeStore) SetTaskIssue(ctx context.Context, id string, issue int64) error {
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	t.IssueNumber = issue
	s.tasks[id] = t
	return nil
}

func (s *fakeStore) PlanCards(ctx context.Context, plan string) ([]Task, error) {
	var out []Task
	for _, t := range s.tasks {
		if t.Parent == plan && t.Source == SourcePlan && t.PlanKey != "" {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *fakeStore) SetTaskPlanRevision(ctx context.Context, id string, revision int) error {
	if t, ok := s.tasks[id]; ok && t.PlanRevision == 0 {
		t.PlanRevision = revision
		s.tasks[id] = t
	}
	return nil
}

func (s *fakeStore) UpdatePlanCardBrief(ctx context.Context, id, objective string) error {
	if t, ok := s.tasks[id]; ok {
		t.Objective = objective
		s.tasks[id] = t
	}
	return nil
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

func (s *fakeStore) TaskByPR(ctx context.Context, repository string, pr int64) (Task, bool, error) {
	cards, _ := s.TasksByPR(ctx, repository, pr)
	if len(cards) == 0 {
		return Task{}, false, nil
	}
	return cards[0], true, nil
}

func (s *fakeStore) TasksByPR(ctx context.Context, repository string, pr int64) ([]Task, error) {
	var out []Task
	for _, t := range s.tasks {
		if t.Repository == repository && t.PRNumber == pr {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b Task) int { return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), strings.Compare(b.ID, a.ID)) })
	return out, nil
}

func (s *fakeStore) TasksByRuling(ctx context.Context, repository, ruling string) ([]Task, error) {
	var out []Task
	for _, t := range s.tasks {
		if t.Repository == repository && t.ShipsRuling == ruling {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b Task) int { return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID)) })
	return out, nil
}

func (s *fakeStore) ReachedReview(ctx context.Context, id string) (bool, error) {
	return slices.ContainsFunc(s.events, func(e Event) bool {
		return e.TaskID == id && e.Kind == EventTaskStatus && e.Detail["status"] == "in-review"
	}), nil
}

func (s *fakeStore) InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error) {
	if s.failIntakeEvent != nil {
		return 0, s.failIntakeEvent
	}
	for _, have := range s.intakeEvents {
		if (e.DeliveryID != "" && have.DeliveryID == e.DeliveryID) ||
			(e.CommentID != 0 && have.TaskID == e.TaskID && have.Kind == e.Kind && have.CommentID == e.CommentID) ||
			(e.Kind == IntakeHandover && have.TaskID == e.TaskID && have.Kind == IntakeHandover) {
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

func (s *fakeStore) ConfirmFactoryClose(ctx context.Context, taskID string, issue int64) error {
	if !slices.ContainsFunc(s.intakeEvents, func(e IntakeEvent) bool {
		return e.TaskID == taskID && e.Kind == IntakeFactoryClosed && e.CommentID == issue
	}) {
		return nil
	}
	if s.confirmedCloses == nil {
		s.confirmedCloses = map[[2]any]bool{}
	}
	s.confirmedCloses[[2]any{taskID, issue}] = true
	return nil
}

func (s *fakeStore) UnconfirmedFactoryCloses(ctx context.Context, olderThan time.Duration, limit int) ([]IntakeEvent, error) {
	var out []IntakeEvent
	for _, e := range s.intakeEvents {
		if e.Kind == IntakeFactoryClosed && !s.confirmedCloses[[2]any{e.TaskID, e.CommentID}] && time.Since(e.CreatedAt) > olderThan && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *fakeStore) ReleaseHandover(ctx context.Context, taskID string) error {
	s.intakeEvents = slices.DeleteFunc(s.intakeEvents, func(e IntakeEvent) bool {
		return e.TaskID == taskID && e.Kind == IntakeHandover
	})
	return nil
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

func (s *fakeStore) TouchedByPerson(ctx context.Context, taskID string, bots ...string) (bool, error) {
	for _, e := range s.intakeEvents {
		if e.TaskID == taskID && e.Author != FactoryDecider && !slices.Contains(bots, e.Author) {
			return true, nil
		}
	}
	return slices.ContainsFunc(s.events, func(e Event) bool { return e.TaskID == taskID && e.Kind == EventQuestionAnswered }), nil
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
		return fmt.Errorf("task %s: %w (not in %v, cannot move to %s)", id, ErrNotFound, from, to)
	}
	t.Status = to
	if Final(to) || to == "merged" {
		now := time.Now()
		if s.chainNow != nil {
			now = s.chainNow()
		}
		t.FinishedAt = &now
		if Final(to) {
			s.finished[id] = now
			t.Gate, t.GateSince = "", nil
		}
	}
	s.tasks[id] = t
	return nil
}

func (s *fakeStore) MarkStarted(ctx context.Context, id string) error {
	t, ok := s.tasks[id]
	if !ok || !slices.Contains([]string{"queued", "waiting-deps", "running", "in-review", "waiting-human", "waiting-poster"}, t.Status) {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	t.Status = "running"
	s.tasks[id] = t
	s.started[id] = true
	return nil
}

func (s *fakeStore) SetAdmitted(ctx context.Context, id string) error {
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	if t.AdmittedAt == nil {
		now := time.Now()
		t.AdmittedAt = &now
		s.tasks[id] = t
	}
	return nil
}

func (s *fakeStore) CountAdmitted(ctx context.Context, repository string) (int, error) {
	n := 0
	for _, t := range s.tasks {
		if t.Repository != repository || t.Kind != "agent" || t.Lane == "" {
			continue
		}
		if slices.Contains([]string{"waiting-deps", "running", "in-review", "closing"}, t.Status) ||
			(t.Status == "queued" && t.AdmittedAt != nil) {
			n++
		}
	}
	return n, nil
}

func (s *fakeStore) CountBacklog(ctx context.Context, repository string) (int, error) {
	n := 0
	for _, t := range s.tasks {
		if t.Repository != repository || t.Kind != "agent" || t.Lane == "" || t.AdmittedAt != nil {
			continue
		}
		if !factorydFinal(t.Status) {
			n++
		}
	}
	return n, nil
}

// factorydFinal is Final for the fake, which cannot import its own
// package's helper by name.
func factorydFinal(status string) bool {
	switch status {
	case "done", "failed", "closed":
		return true
	}
	return false
}

func (s *fakeStore) ListQueuedByRepository(ctx context.Context, repository string) ([]Task, error) {
	var out []Task
	for _, t := range s.tasks {
		if t.Repository == repository && t.Kind == "agent" && t.Lane != "" && t.Status == "queued" {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b Task) int {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Compare(b.CreatedAt)
		}
		// same second: the issue number is the filing sequence, as the
		// SQL's ORDER BY; 0 means no issue and sorts last
		if a.IssueNumber != b.IssueNumber {
			if a.IssueNumber == 0 {
				return 1
			}
			if b.IssueNumber == 0 {
				return -1
			}
			return cmp.Compare(a.IssueNumber, b.IssueNumber)
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
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
var testSeeded = map[string]string{policy.SecretCIRegistrar: "registrar-tok", policy.SecretMerge: "merge-tok", policy.SecretConfig: "config-tok",
	policy.SecretGate: "gate-tok"}

// seedProject is repository as the code repository's project: seed, agents
// coder, the registrar token reading and running its Actions, the merge
// token merging, the gate token approving the merge gate's merges.
func seedProject(repository string) Project {
	return Project{Name: strings.ReplaceAll(repository, "/", "-"), Repository: repository, MergeRule: "human", Objects: "seed", Agents: "coder",
		File: true, Tokens: policy.SeedTokens{Read: policy.SecretCIRegistrar, Actions: policy.SecretCIRegistrar, Merge: policy.SecretMerge, Gate: policy.SecretGate},
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

// Record is the Timeline's sink: rows land newest last, as the webhook's
// doc_written rows are read back.
func (s *fakeStore) Record(ctx context.Context, e Event) (int64, error) {
	s.events = append(s.events, e)
	return int64(len(s.events)), nil
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

// NewestTaskEvents is the newest event of each of tasks, newest row last.
func (s *fakeStore) NewestTaskEvents(ctx context.Context, tasks []string) (map[string]Event, error) {
	set := map[string]bool{}
	for _, t := range tasks {
		set[t] = true
	}
	out := map[string]Event{}
	for i := len(s.events) - 1; i >= 0; i-- {
		e := s.events[i]
		if e.TaskID != "" && set[e.TaskID] {
			if _, ok := out[e.TaskID]; !ok {
				out[e.TaskID] = e
			}
		}
	}
	return out, nil
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

func (s *fakeStore) SetTaskAlertFingerprint(ctx context.Context, id, fingerprint string) error {
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	t.AlertFingerprint = fingerprint
	s.tasks[id] = t
	return nil
}

func (s *fakeStore) SetTaskMergeCommit(ctx context.Context, id, commit string) error {
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	if t.MergeCommit == "" {
		t.MergeCommit = commit
		s.tasks[id] = t
	}
	return nil
}

func (s *fakeStore) OpenApproval(ctx context.Context, a ApprovalRequest) (int64, error) {
	for i := range s.approvals {
		if (a.Kind == ApprovalMerge || a.Kind == ApprovalPlan) && s.approvals[i].TaskID == a.TaskID && s.approvals[i].Kind == a.Kind && s.approvals[i].Status == "pending" {
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

func (s *fakeStore) DecideApproval(ctx context.Context, id int64, kind, status, by, note string) error {
	for i, a := range s.approvals {
		if a.ID == id && a.Kind == kind && a.Status == "pending" {
			s.approvals[i].Status, s.approvals[i].DecidedBy, s.approvals[i].DecisionNote = status, by, note
			return nil
		}
	}
	return ErrNotFound
}

func (s *fakeStore) UndecideApproval(ctx context.Context, id int64) error {
	for i := range s.approvals {
		if a := &s.approvals[i]; a.ID == id {
			a.Status, a.DecidedBy, a.DecisionNote = "pending", "", ""
		}
	}
	return nil
}

func (s *fakeStore) ReopenApproval(ctx context.Context, taskID, kind, note string) error {
	for i := len(s.approvals) - 1; i >= 0; i-- {
		if a := &s.approvals[i]; a.TaskID == taskID && a.Kind == kind {
			if a.Status != "denied" {
				a.Status, a.DecidedBy, a.DecisionNote = "pending", "", note
			}
			return nil
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
	if a.Kind == AlertScreening && slices.ContainsFunc(s.alerts, func(b Alert) bool { return b.Kind == AlertScreening && b.TaskID == a.TaskID && b.Verdict == "" }) {
		return 0, nil
	}
	if a.Kind == AlertMonthCap && slices.ContainsFunc(s.alerts, func(b Alert) bool { return b.Kind == AlertMonthCap && b.Fingerprint == a.Fingerprint }) {
		return 0, nil
	}
	if (a.Kind == AlertChainRed || a.Kind == AlertChainStalled || a.Kind == AlertChainUncarried) && slices.ContainsFunc(s.alerts, func(b Alert) bool {
		return b.Kind == a.Kind && b.Fingerprint == a.Fingerprint && b.Verdict == ""
	}) {
		return 0, nil
	}
	a.ID = int64(len(s.alerts) + 1)
	s.alerts = append(s.alerts, a)
	return a.ID, nil
}

func (s *fakeStore) MonthSpend(ctx context.Context, at time.Time) (MonthSpend, error) {
	return s.monthSpend, nil
}

func (s *fakeStore) CapRefusal(ctx context.Context, taskID string, since time.Time) (*CapRefusal, error) {
	if r, ok := s.capRefusals[taskID]; ok {
		return &r, nil
	}
	return nil, nil
}

func (s *fakeStore) BudgetRefused(ctx context.Context, taskID string, since time.Time) (bool, error) {
	return s.budgetRefused[taskID], nil
}

func (s *fakeStore) ModelAnswered(ctx context.Context, taskID string, since time.Time) (bool, error) {
	return s.answered[taskID], nil
}

func (s *fakeStore) CheckinSpent(ctx context.Context, taskID string) (float64, error) {
	return s.checkinSpent[taskID], nil
}

func (s *fakeStore) CheckinCalls(ctx context.Context, taskID string) (int, error) {
	return s.checkinCalls[taskID], nil
}

func (s *fakeStore) ProviderRefusal(ctx context.Context, taskID string, since time.Time) (string, bool, error) {
	r, ok := s.providerRefusals[taskID]
	return r, ok, nil
}

func (s *fakeStore) Free(ctx context.Context, r policy.HoldRequest) (policy.TaskFree, policy.Free, error) {
	t, ok := s.tasks[r.TaskID]
	if !ok {
		return policy.TaskFree{}, policy.Free{}, fmt.Errorf("hold: no task row for %s", r.TaskID)
	}
	tf := s.held[r.TaskID]
	tf.Status, tf.Headroom = t.Status, t.BudgetEUR-t.SpentEUR
	inf := math.Inf(1)
	f := policy.Free{Day: inf, Month: inf, Project: inf}
	if s.capsFree != nil {
		f = *s.capsFree
	}
	f.Task = tf.Free()
	return tf, f, nil
}

func (s *fakeStore) Opening(ctx context.Context, taskID, ref string) (policy.Opening, error) {
	return s.openings[ref], nil
}

func (s *fakeStore) SetTaskModelAlias(ctx context.Context, id, alias string) error {
	t := s.tasks[id]
	t.ModelAlias = alias
	s.tasks[id] = t
	return nil
}

func (s *fakeStore) PauseQueue(ctx context.Context, p QueuePause) error {
	if s.pauses == nil {
		s.pauses = map[string]QueuePause{}
	}
	if _, ok := s.pauses[p.Project]; !ok {
		s.pauses[p.Project] = p
	}
	return nil
}

func (s *fakeStore) QueuePause(ctx context.Context, project string) (QueuePause, bool, error) {
	p, ok := s.pauses[project]
	return p, ok, nil
}

func (s *fakeStore) SetQueuePauseRevert(ctx context.Context, project, taskID string) error {
	if p, ok := s.pauses[project]; ok {
		p.RevertTask = taskID
		s.pauses[project] = p
	}
	return nil
}

func (s *fakeStore) ClearQueuePause(ctx context.Context, project string) error {
	delete(s.pauses, project)
	return nil
}

func (s *fakeStore) RoutineState(ctx context.Context, project, routine string) (RoutineState, error) {
	k := project + "/" + routine
	st, ok := s.routineStates[k]
	if !ok {
		st = RoutineState{Project: project, Routine: routine}
		s.routineStates[k] = st
	}
	return st, nil
}

func (s *fakeStore) RoutineStates(ctx context.Context, project string) ([]RoutineState, error) {
	var out []RoutineState
	for _, st := range s.routineStates {
		if st.Project == project {
			out = append(out, st)
		}
	}
	slices.SortFunc(out, func(a, b RoutineState) int { return strings.Compare(a.Routine, b.Routine) })
	return out, nil
}

func (s *fakeStore) SetRoutineMerges(ctx context.Context, project, routine string, merges int) error {
	st, _ := s.RoutineState(ctx, project, routine)
	st.Merges = merges
	s.routineStates[project+"/"+routine] = st
	return nil
}

func (s *fakeStore) SetRoutineSkip(ctx context.Context, project, routine string, skip bool, by string) error {
	st, _ := s.RoutineState(ctx, project, routine)
	st.SkipNext, st.SkipBy = skip, by
	if !skip {
		st.SkipBy = ""
	}
	s.routineStates[project+"/"+routine] = st
	return nil
}

func (s *fakeStore) SetRoutineNeedsMe(ctx context.Context, project, routine string, needsMe *bool, by string) error {
	st, _ := s.RoutineState(ctx, project, routine)
	st.NeedsMe, st.NeedsMeBy = needsMe, by
	if needsMe == nil {
		st.NeedsMeBy = ""
	}
	s.routineStates[project+"/"+routine] = st
	return nil
}

func (s *fakeStore) BeginRoutineRun(ctx context.Context, r RoutineRun, skip bool) (RoutineRun, bool, error) {
	k := r.Project + "/" + r.Routine
	for _, have := range s.routineRuns[k] {
		if have.Due.Equal(r.Due) {
			return have, false, nil
		}
	}
	st, _ := s.RoutineState(ctx, r.Project, r.Routine)
	st.Runs++
	run := RoutineRun{Project: r.Project, Routine: r.Routine, Run: st.Runs, Due: r.Due, Started: time.Now(), Trigger: r.Trigger}
	if skip && st.SkipNext {
		run.Outcome, run.Note = "skipped", "skipped once by "+st.SkipBy
		st.SkipNext, st.SkipBy = false, ""
	}
	s.routineStates[k] = st
	s.routineRuns[k] = append(s.routineRuns[k], run)
	return run, true, nil
}

func (s *fakeStore) RoutineRunFiled(ctx context.Context, project, routine string, run int, taskID string, issue int64) error {
	runs := s.routineRuns[project+"/"+routine]
	for i := range runs {
		if runs[i].Run == run && runs[i].Ended.IsZero() {
			runs[i].Outcome, runs[i].TaskID, runs[i].Issue = "filed", taskID, issue
			return nil
		}
	}
	return fmt.Errorf("routine run %s/%s #%d: %w", project, routine, run, ErrNotFound)
}

func (s *fakeStore) EndRoutineRun(ctx context.Context, project, routine string, run int, outcome, note, taskID string, issue int64, costEUR float64) error {
	runs := s.routineRuns[project+"/"+routine]
	for i := range runs {
		if runs[i].Run == run {
			runs[i].Ended, runs[i].Outcome, runs[i].Note, runs[i].TaskID, runs[i].Issue, runs[i].CostEUR = time.Now(), outcome, note, taskID, issue, costEUR
			return nil
		}
	}
	return fmt.Errorf("routine run %s/%s #%d: %w", project, routine, run, ErrNotFound)
}

func (s *fakeStore) RepointRoutineRun(ctx context.Context, taskID, newTaskID string, issue int64) error {
	for _, runs := range s.routineRuns {
		for i := range runs {
			if runs[i].TaskID == taskID && runs[i].Ended.IsZero() {
				runs[i].TaskID, runs[i].Issue = newTaskID, issue
				return nil
			}
		}
	}
	return nil
}

func (s *fakeStore) RoutineRuns(ctx context.Context, project, routine string, limit int) ([]RoutineRun, error) {
	runs := slices.Clone(s.routineRuns[project+"/"+routine])
	slices.Reverse(runs)
	return runs[:min(limit, len(runs))], nil
}

func (s *fakeStore) RoutineRunOfTask(ctx context.Context, taskID string) (RoutineRun, bool, error) {
	for _, runs := range s.routineRuns {
		for _, r := range runs {
			if taskID != "" && r.TaskID == taskID {
				return r, true, nil
			}
		}
	}
	return RoutineRun{}, false, nil
}

func (s *fakeStore) OpenRoutineTask(ctx context.Context, project, routine string) (Task, bool, error) {
	runs := s.routineRuns[project+"/"+routine]
	for i := len(runs) - 1; i >= 0; i-- {
		if t, ok := s.tasks[runs[i].TaskID]; ok && !Final(t.Status) {
			return t, true, nil
		}
	}
	return Task{}, false, nil
}

func (s *fakeStore) RoutineMergeCursor(ctx context.Context) (int64, error) {
	return s.routineCursor, nil
}

func (s *fakeStore) CountRoutineMerges(ctx context.Context, through int64, merged map[string]int) error {
	for k, st := range s.routineStates {
		st.Merges += merged[st.Project]
		s.routineStates[k] = st
	}
	s.routineCursor = through
	return nil
}

// Project is the project of s.projects named name.
func (s *fakeStore) Project(ctx context.Context, name string) (Project, bool, error) {
	i := slices.IndexFunc(s.projects, func(p Project) bool { return p.Name == name })
	if i < 0 {
		return Project{}, false, nil
	}
	return s.projects[i], true, nil
}

func (s *fakeStore) PutLiveSession(ctx context.Context, l LiveSession) error {
	if s.liveSessions == nil {
		s.liveSessions = map[string]LiveSession{}
	}
	s.liveSessions[l.TaskID] = l
	return nil
}

func (s *fakeStore) SetLiveSessionLines(ctx context.Context, taskID string, lines int) error {
	if l, ok := s.liveSessions[taskID]; ok {
		l.Lines = lines
		s.liveSessions[taskID] = l
	}
	return nil
}

func (s *fakeStore) LiveSession(ctx context.Context, taskID string) (LiveSession, bool, error) {
	l, ok := s.liveSessions[taskID]
	return l, ok, nil
}

func (s *fakeStore) DeleteLiveSession(ctx context.Context, taskID string) error {
	delete(s.liveSessions, taskID)
	return nil
}

func (s *fakeStore) SetRollHold(ctx context.Context, taskID string, since, until time.Time) error {
	if s.holds == nil {
		s.holds = map[string]RollHold{}
	}
	s.holds[taskID] = RollHold{TaskID: taskID, Since: since, Until: until}
	return nil
}

func (s *fakeStore) ClearRollHold(ctx context.Context, taskID string) error {
	delete(s.holds, taskID)
	return nil
}

func (s *fakeStore) RollHold(ctx context.Context, now time.Time) (RollHold, bool, error) {
	var newest RollHold
	for _, h := range s.holds {
		if h.Until.After(now) && h.Since.After(newest.Since) {
			newest = h
		}
	}
	return newest, newest.TaskID != "", nil
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

func (s *fakeStore) NewestAlertRows(ctx context.Context, kind, namespace string) ([]Alert, error) {
	s.alertsMu.Lock()
	defer s.alertsMu.Unlock()
	newest := map[string]int{}
	for i, a := range s.alerts {
		if a.Kind == kind && a.Fingerprint != "" {
			newest[a.Fingerprint] = i
		}
	}
	var out []Alert
	for i, a := range s.alerts {
		if a.Kind == kind && a.Fingerprint != "" && newest[a.Fingerprint] == i && (namespace == "" || a.Detail["namespace"] == namespace) {
			a.Detail = maps.Clone(a.Detail)
			out = append(out, a)
		}
	}
	return out, nil
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

func (s *fakeStore) JudgeEscalations(ctx context.Context, taskID string, filter EscalationFilter, verdict, by string) error {
	s.alertsMu.Lock()
	defer s.alertsMu.Unlock()
	for i, a := range s.alerts {
		if a.TaskID == taskID && a.Kind == AlertEscalation && a.Verdict == "" && (filter.Source == "" || a.Source == filter.Source) &&
			(filter.Comment == 0 || fmt.Sprint(a.Detail["comment"]) == fmt.Sprint(filter.Comment)) {
			now := time.Now()
			a.Verdict, a.ResolvedBy, a.ResolvedAt = verdict, by, &now
			s.alerts[i] = a
		}
	}
	return nil
}

func (s *fakeStore) DayFree(ctx context.Context, dayEUR float64) (float64, error) {
	return dayEUR - s.daySpent, nil
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
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events, ledger, holds, approval_requests CASCADE`); err != nil {
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
	if got, _, _ := s.Task(ctx, "t1"); got.PRNumber != 12 {
		t.Fatalf("pr_number = %d, want 12", got.PRNumber)
	}
	usage := TaskUsage{AgentTime: 90 * time.Second, Attempts: 2, ExtraTime: time.Minute, RetryTime: 2 * time.Minute, ExtraAttempts: 1, PosterAttempts: 1, LastUncounted: true, ExtraCheckin: 1, ExtraCheckinShare: 2}
	if err := s.SetTaskUsage(ctx, "t1", usage); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "t1"); got.Usage != usage {
		t.Fatalf("usage = %+v, want %+v", got.Usage, usage)
	}
	if got, _, _ := s.Task(ctx, "t1"); got.Usage.ExtraCheckinShare != 2 {
		t.Fatalf("extra_checkin_share = %d, want 2: the granted check-in share persists on the row", got.Usage.ExtraCheckinShare)
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
		if _, err := s.db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur) VALUES ($1, 'model', 'small', 0.5)`, row.id); err != nil {
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
	if _, err := s.db.ExecContext(ctx, `INSERT INTO holds (task_id, kind, ref, eur, expires_at) VALUES ('s-failed', 'model', 'small', 0.1, now() + interval '1 hour')`); err != nil {
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
	if _, err := s.db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur) VALUES ('s-tool', 'model', 'small', 0.75), ('s-tool', 'tool', 'web_scrape', 0.25)`); err != nil {
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
	if _, err := s.db.ExecContext(ctx, `INSERT INTO ledger (task_id, day, kind, ref, eur) VALUES ('s-cut', current_date - 1, 'model', 'small', 0.1873), ('s-cut', current_date - 1, 'model', 'review/big', 0.1447), ('s-cut', current_date - 1, 'model', 'review/big', 0.1447)`); err != nil {
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
	// (the rows carry their finish time: the finished check refuses a
	// final row without one)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM backfills WHERE name = '0014_task_capabilities';
		DELETE FROM schema_migrations WHERE name = 'factoryd/0014_task_capabilities.sql';
		INSERT INTO tasks (id, budget_eur, status, kind, required_capabilities, finished_at) VALUES
		('b-agent', 1, 'done', 'agent', 'null', now()), ('b-human', 1, 'done', 'human', 'null', now())`); err != nil {
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

// A build card's Ships data reaches the store and the review gate's
// read of every card citing the ruling: the parsed ruling and clauses
// round-trip through CreateTask's plain INSERT — the conflict clauses
// insertTask appends (report, plan and chat cards) are all DO NOTHING
// and none of those kinds ever carries a Ships line — and TasksByRuling
// lists the citing cards oldest first, whatever their status.
// Re-creating an existing card is a loud unique violation, never a
// silent upsert that could drop its stated clauses.
func TestPGStoreShipsRuling(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks CASCADE`); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	for _, c := range []Task{
		{ID: "t-band", Status: "done", Repository: "sf/r", Kind: "agent", Source: "webhook", CreatedAt: at, ShipsRuling: "C20", ShipsClauses: []int{1, 3}},
		{ID: "t-panel", Status: "queued", Repository: "sf/r", Kind: "agent", Source: "webhook", CreatedAt: at.Add(time.Hour), ShipsRuling: "C20", ShipsClauses: []int{2}},
		{ID: "t-uncited", Status: "queued", Repository: "sf/r", Kind: "agent", Source: "webhook", CreatedAt: at.Add(2 * time.Hour)},
	} {
		if err := s.CreateTask(ctx, c); err != nil {
			t.Fatalf("CreateTask %s: %v", c.ID, err)
		}
	}
	if got, _, _ := s.Task(ctx, "t-band"); got.ShipsRuling != "C20" || !reflect.DeepEqual(got.ShipsClauses, []int{1, 3}) {
		t.Fatalf("t-band ships %q %v, want C20 [1 3]", got.ShipsRuling, got.ShipsClauses)
	}
	citing, err := s.TasksByRuling(ctx, "sf/r", "C20")
	if err != nil {
		t.Fatal(err)
	}
	if len(citing) != 2 || citing[0].ID != "t-band" || citing[1].ID != "t-panel" {
		var ids []string
		for _, c := range citing {
			ids = append(ids, c.ID)
		}
		t.Fatalf("TasksByRuling = %v, want [t-band t-panel]", ids)
	}
	dup := Task{ID: "t-band", Status: "queued", Repository: "sf/r", Kind: "agent", Source: "webhook", ShipsRuling: "C20", ShipsClauses: []int{1, 2, 3}}
	if err := s.CreateTask(ctx, dup); err == nil {
		t.Fatal("re-creating t-band was accepted, want a unique violation")
	}
	if got, _, _ := s.Task(ctx, "t-band"); !reflect.DeepEqual(got.ShipsClauses, []int{1, 3}) {
		t.Fatalf("t-band clauses after the refused re-create = %v, want [1 3]", got.ShipsClauses)
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
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events, approval_requests CASCADE`); err != nil {
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
	// the workflow's resolve leaves the console's decision; a reopen does not
	a3, err := s.OpenApproval(ctx, ApprovalRequest{Kind: "merge", TaskID: "a3", Project: "sf/r", PRNumber: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DecideApproval(ctx, a3, "merge", "approved", "sebastian", "approved in the console"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveApprovals(ctx, "a3", "merge", "approved", "factoryd", "merged"); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.ApprovalRequests(ctx, "approved"); len(a) != 2 || a[1].ID != a3 || a[1].DecidedBy != "sebastian" || a[1].DecisionNote != "approved in the console" {
		t.Fatalf("approved after the workflow's resolve = %+v", a)
	}
	if err := s.ReopenApproval(ctx, "a3", "merge", "waiting: Forgejo refused the merge"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.ApprovalRequests(ctx, "pending"); len(p) != 1 || p[0].ID != a3 || p[0].DecisionNote != "waiting: Forgejo refused the merge" || p[0].DecidedBy != "" || p[0].DecidedAt != nil {
		t.Fatalf("reopened = %+v", p)
	}
	if err := s.ResolveApprovals(ctx, "a3", "", "denied", "factoryd", "done"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReopenApproval(ctx, "a3", "merge", "again"); err != nil {
		t.Fatal(err)
	}
	// a decision whose signal failed is undone, denied or not
	if err := s.UndecideApproval(ctx, a3); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.ApprovalRequests(ctx, "pending"); len(p) != 1 || p[0].ID != a3 || p[0].DecidedBy != "" || p[0].DecisionNote != "" || p[0].DecidedAt != nil {
		t.Fatalf("undone = %+v", p)
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

// DeployedCommit (roadmap 194): the commit of a deployment's newest
// recorded roll — the tree that was live when an alert on it fires, which
// a signal card's filing pins as its reproduction base — and ok false for
// a deployment with no roll.
func TestPGStoreDeployedCommit(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE rolls`); err != nil {
		t.Fatal(err)
	}
	if sha, ok, err := s.DeployedCommit(ctx, "factoryd"); err != nil || ok || sha != "" {
		t.Fatalf("no roll recorded: %q %v %v", sha, ok, err)
	}
	base := Roll{SHA: "roll-7", Image: "worker", Deployment: "factoryd",
		PushedAt: at("2026-10-13T06:00:00Z"), AppliedAt: at("2026-10-13T06:02:00Z"), LiveAt: at("2026-10-13T06:04:00Z")}
	if ok, err := s.RecordRoll(ctx, base); err != nil || !ok {
		t.Fatalf("record roll-7: %v %v", ok, err)
	}
	newer := base
	newer.SHA, newer.Image = "roll-8", "gateway"
	newer.PushedAt, newer.AppliedAt, newer.LiveAt = at("2026-10-14T06:00:00Z"), at("2026-10-14T06:02:00Z"), at("2026-10-14T06:04:00Z")
	if ok, err := s.RecordRoll(ctx, newer); err != nil || !ok {
		t.Fatalf("record roll-8: %v %v", ok, err)
	}
	if sha, ok, err := s.DeployedCommit(ctx, "factoryd"); err != nil || !ok || sha != "roll-8" {
		t.Fatalf("deployed commit = %q %v %v, want roll-8", sha, ok, err)
	}
	if sha, ok, err := s.DeployedCommit(ctx, "factory-gateway"); err != nil || ok || sha != "" {
		t.Fatalf("another deployment: %q %v %v", sha, ok, err)
	}
}

// Migration 0026 on a real Postgres: the author and the agent's summary
// on the row, a backlog card outside every gate until it enters a lane, closed
// as a final state, the info_request alert kind, projects by repository.
// The intake switch's rows: no row reads open, a set writes the state,
// and a second set flips it back.
func TestPGStoreChatIntake(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE chat_intake`); err != nil {
		t.Fatal(err)
	}
	if closed, err := s.ChatIntakeClosed(ctx, "sf/code"); err != nil || closed {
		t.Fatalf("no row reads %v, %v; want open", closed, err)
	}
	if err := s.SetChatIntake(ctx, "sf/code", true, "sebastian", time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if closed, err := s.ChatIntakeClosed(ctx, "sf/code"); err != nil || !closed {
		t.Fatalf("the closed row reads %v, %v; want closed", closed, err)
	}
	if err := s.SetChatIntake(ctx, "sf/code", false, "sebastian", time.Now()); err != nil {
		t.Fatal(err)
	}
	if closed, err := s.ChatIntakeClosed(ctx, "sf/code"); err != nil || closed {
		t.Fatalf("the reopened row reads %v, %v; want open", closed, err)
	}
}

func TestPGStoreIntakeAuto(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events, ledger, holds, approval_requests, alerts CASCADE`); err != nil {
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

// The plan_holdouts table on a real Postgres (the spec-intake concept,
// piece 6): a round's drafts store once, the human's okay and drop decide
// only the states they take, the sweep drops what no click can reach, the
// audit fields and the rewrite move as the audit sessions drive them, and
// the past reverts are the project's own last failures, one line each.
func TestPGStoreHoldouts(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, plan_questions, plan_holdouts CASCADE`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, Task{ID: "t-ho", Status: "queued", Objective: "o", Repository: "sf/r", Kind: "agent", Source: "issue"}); err != nil {
		t.Fatal(err)
	}
	drafts := []Holdout{
		{Idx: 1, ID: "door_state", Origin: HoldoutDrafted, Criterion: "The door opens only for a key that fits",
			State: HoldoutProposed, TestName: "TestDoorShut", TestCode: "package holdout", DryHead: "abc",
			DryVerdict: "red", DryOutput: "--- FAIL: TestDoorShut (0.01s)"},
		{Idx: 2, ID: "key_log", Criterion: "Every key turn lands in the log",
			State: HoldoutUnrunnable, TestName: "TestKeyLog", DryVerdict: "unrunnable"},
	}
	if ok, err := s.PutPlanHoldouts(ctx, "t-ho", 1, drafts); err != nil || !ok {
		t.Fatalf("first put: %v %v", ok, err)
	}
	if ok, err := s.PutPlanHoldouts(ctx, "t-ho", 1, drafts); err != nil || ok {
		t.Fatalf("put over a round with rows: %v %v, want false", ok, err)
	}
	rows, err := s.PlanHoldouts(ctx, "t-ho")
	if err != nil || len(rows) != 2 || rows[0].Idx != 1 || rows[1].Idx != 2 {
		t.Fatalf("rows = %v, %v; want two in idx order", rows, err)
	}
	if r := rows[0]; r.Round != 1 || r.ID != "door_state" || r.Origin != HoldoutDrafted || r.State != HoldoutProposed ||
		r.TestName != "TestDoorShut" || r.TestCode != "package holdout" || r.DryHead != "abc" ||
		r.DryVerdict != "red" || r.DryOutput != "--- FAIL: TestDoorShut (0.01s)" || r.AuditVerdict != "" || r.RedCycles != 0 {
		t.Fatalf("row 1 = %+v; want the draft as stored", r)
	}
	if n, err := s.UndecidedHoldouts(ctx, "t-ho", 1); err != nil || n != 2 {
		t.Fatalf("undecided = %d, %v; want 2", n, err)
	}

	// The clicks: okay takes a proposal with a dry verdict, drop takes a
	// proposal and an unrunnable, and every other click on an existing row
	// is ErrHoldoutClick, on no row ErrNoHoldout.
	if err := s.DecidePlanHoldout(ctx, "t-ho", 1, 1, HoldoutStored); err != nil {
		t.Fatalf("okay door_state: %v", err)
	}
	for _, tc := range []struct {
		round, idx int
		state      string
		want       error
	}{
		{1, 1, HoldoutStored, ErrHoldoutClick},  // decided already
		{1, 2, HoldoutStored, ErrHoldoutClick},  // unrunnable takes no okay
		{2, 1, HoldoutDropped, ErrHoldoutClick}, // the idx belongs to round 1
		{1, 9, HoldoutDropped, ErrNoHoldout},
	} {
		if err := s.DecidePlanHoldout(ctx, "t-ho", tc.round, tc.idx, tc.state); !errors.Is(err, tc.want) {
			t.Errorf("decide %d/%d %s = %v, want %v", tc.round, tc.idx, tc.state, err, tc.want)
		}
	}
	if err := s.DecidePlanHoldout(ctx, "t-ho", 1, 2, HoldoutDropped); err != nil {
		t.Fatalf("drop key_log: %v", err)
	}
	if n, err := s.UndecidedHoldouts(ctx, "t-ho", 1); err != nil || n != 0 {
		t.Fatalf("undecided after the clicks = %d, %v; want 0", n, err)
	}

	// The audit fields on the stored row: filed once, ErrAnswered after;
	// opened is public and idempotent; the clear lets a new audit file; a
	// rewrite rides on the row until the gate runs it as the test.
	if err := s.SetHoldoutAudit(ctx, "t-ho", 1, AuditRealGap, "the log never lands", ""); err != nil {
		t.Fatalf("first audit: %v", err)
	}
	if err := s.SetHoldoutAudit(ctx, "t-ho", 1, AuditRealGap, "again", ""); !errors.Is(err, ErrAnswered) {
		t.Errorf("second audit = %v, want ErrAnswered", err)
	}
	if id, err := s.OpenHoldout(ctx, "t-ho", 1); err != nil || id != "door_state" {
		t.Fatalf("open = %q, %v; want door_state", id, err)
	}
	if id, err := s.OpenHoldout(ctx, "t-ho", 1); err != nil || id != "door_state" {
		t.Fatalf("open again = %q, %v; want door_state again", id, err)
	}
	if err := s.ClearHoldoutAudit(ctx, "t-ho", 1); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := s.SetHoldoutAudit(ctx, "t-ho", 1, AuditWrongTest, "the test names the hinge", "package holdout\nfunc TestDoorShut2(t *testing.T)"); err != nil {
		t.Fatalf("audit after the clear: %v", err)
	}
	if rows, _ = s.PlanHoldouts(ctx, "t-ho"); rows[0].State != HoldoutOpened || rows[0].Rewrite == "" {
		t.Fatalf("row 1 after the rewrite = %+v; want still opened with the rewrite", rows[0])
	}
	if err := s.SetHoldoutTest(ctx, "t-ho", 1, "package holdout v2", "red", "FAIL: the hinge moved"); err != nil {
		t.Fatalf("set the rewrite as the test: %v", err)
	}
	if rows, _ = s.PlanHoldouts(ctx, "t-ho"); rows[0].TestCode != "package holdout v2" || rows[0].DryOutput != "FAIL: the hinge moved" ||
		rows[0].AuditVerdict != "" || rows[0].Rewrite != "" {
		t.Fatalf("row 1 after the set = %+v; want the rewrite as the test, audit fields empty", rows[0])
	}
	for i := 1; i <= 2; i++ {
		if n, err := s.BumpHoldoutRed(ctx, "t-ho", 1); err != nil || n != i {
			t.Fatalf("red cycle %d = %d, %v", i, n, err)
		}
	}
	for _, fn := range []struct {
		name string
		call func() error
	}{
		{"bump", func() error { _, err := s.BumpHoldoutRed(ctx, "t-ho", 9); return err }},
		{"open", func() error { _, err := s.OpenHoldout(ctx, "t-ho", 9); return err }},
		{"audit", func() error { return s.SetHoldoutAudit(ctx, "t-ho", 9, AuditRealGap, "", "") }},
	} {
		if err := fn.call(); !errors.Is(err, ErrNoHoldout) {
			t.Errorf("%s on no row = %v, want ErrNoHoldout", fn.name, err)
		}
	}

	// A wrong test with no rewrite drops the hold-out with its verdict.
	if ok, err := s.PutPlanHoldouts(ctx, "t-ho", 2, []Holdout{{Idx: 3, ID: "late", Criterion: "A late one",
		State: HoldoutProposed, TestName: "TestLate", DryVerdict: "green"}}); err != nil || !ok {
		t.Fatalf("round 2 put: %v %v", ok, err)
	}
	if err := s.DecidePlanHoldout(ctx, "t-ho", 2, 3, HoldoutStored); err != nil {
		t.Fatalf("okay the late one: %v", err)
	}
	if err := s.SetHoldoutAudit(ctx, "t-ho", 3, AuditWrongTest, "no rewrite drops it", ""); err != nil {
		t.Fatalf("wrong test: %v", err)
	}
	if rows, _ = s.PlanHoldouts(ctx, "t-ho"); rows[2].State != HoldoutDropped || rows[2].AuditVerdict != AuditWrongTest {
		t.Fatalf("row 3 = %+v; want dropped with its verdict", rows[2])
	}

	// The sweep drops every undecided row of the task, and the past reverts
	// are the repository's last three, first lines only, newest first.
	if ok, err := s.PutPlanHoldouts(ctx, "t-ho", 3, []Holdout{
		{Idx: 4, ID: "sweep_me", Criterion: "swept", State: HoldoutProposed, DryVerdict: "red"},
		{Idx: 5, ID: "sweep_too", Criterion: "swept too", State: HoldoutUnrunnable},
	}); err != nil || !ok {
		t.Fatalf("round 3 put: %v %v", ok, err)
	}
	if err := s.SweepPlanHoldouts(ctx, "t-ho"); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	rows, _ = s.PlanHoldouts(ctx, "t-ho")
	for _, r := range rows {
		if r.State == HoldoutProposed || r.State == HoldoutUnrunnable {
			t.Errorf("row %d still %s after the sweep", r.Idx, r.State)
		}
	}
	for i, o := range []string{"oldest revert stays out", "first revert line\nbody dropped", "second revert line",
		"\nonly the second line"} {
		if err := s.CreateTask(ctx, Task{ID: fmt.Sprintf("t-r%d", i), Status: "closed", Objective: o,
			Repository: "sf/r", Kind: "agent", Source: "revert"}); err != nil {
			t.Fatal(err)
		}
	}
	if lines, err := s.PastReverts(ctx, "sf/r"); err != nil || !slices.Equal(lines, []string{"second revert line", "first revert line"}) {
		t.Fatalf("past reverts = %q, %v; want the last three's first lines, newest first, blanks skipped", lines, err)
	}
}

func TestTouchedByPerson(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, intake_events, events CASCADE`); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"t-bot", "t-member", "t-park", "t-none"} {
		if err := s.CreateTask(ctx, Task{ID: id, Status: "waiting-human", Objective: "o", Repository: "sf/r", Kind: "human", IssueNumber: int64(10 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []IntakeEvent{
		{TaskID: "t-bot", Kind: "issue_comment", Author: "factory-bot", CommentID: 1},
		{TaskID: "t-bot", Kind: IntakeEscalation, Author: FactoryDecider, CommentID: 2},
		{TaskID: "t-member", Kind: "issue_comment", Author: "factory-bot", CommentID: 3},
		{TaskID: "t-member", Kind: "issue_comment", Author: "sebastian", CommentID: 4},
	} {
		if _, err := s.InsertIntakeEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Record(ctx, Event{Repository: "sf/r", TaskID: "t-park", Kind: EventQuestionAnswered, Actor: "sebastian", Via: ViaConsole}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"t-bot": false, "t-member": true, "t-park": true, "t-none": false} {
		if got, err := s.TouchedByPerson(ctx, id, "factory-bot"); err != nil || got != want {
			t.Errorf("TouchedByPerson(%s) = (%v, %v), want %v", id, got, err, want)
		}
	}
}

// The withdraws column on a real Postgres: a withdrawal's intake event
// keeps the escalation it withdraws beside its own comment, once however
// often it is recorded, and every other event withdraws nothing; read
// back, it ends the escalation's answers at its comment: 650 answers 601,
// 750 nothing.
func TestPGStoreIntakeWithdraws(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, intake_events CASCADE`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, Task{ID: "t-wd", Status: "in-review", Objective: "o", Repository: "sf/r", Kind: "agent", IssueNumber: 9}); err != nil {
		t.Fatal(err)
	}
	withdrawal := IntakeEvent{TaskID: "t-wd", Kind: IntakeEscalationWithdrawn, Author: FactoryDecider, Body: "the checks failed", CommentID: 701, Withdraws: 601}
	for _, e := range []IntakeEvent{
		{TaskID: "t-wd", Kind: IntakeEscalation, Author: FactoryDecider, Body: "The gate supervisor stopped ModelReview (attempt 1) past its bound of 10m0s: hangs", CommentID: 601},
		{TaskID: "t-wd", Kind: "issue_comment", Author: "sebastian", Body: "gateway restarted", CommentID: 650, Trusted: true},
		withdrawal, withdrawal,
		{TaskID: "t-wd", Kind: "issue_comment", Author: "sebastian", Body: "by the way", CommentID: 750, Trusted: true},
	} {
		if _, err := s.InsertIntakeEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// the row's time is the database's: the answer's date is set, not today's
	if _, err := s.db.ExecContext(ctx, `UPDATE intake_events SET created_at = '2026-10-15T13:30:00Z' WHERE comment_id = 650`); err != nil {
		t.Fatal(err)
	}
	events, err := s.IntakeEvents(ctx, "t-wd")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		got = append(got, fmt.Sprintf("%s %d withdraws %d", e.Kind, e.CommentID, e.Withdraws))
	}
	want := []string{"escalation 601 withdraws 0", "issue_comment 650 withdraws 0", "escalation_withdrawn 701 withdraws 601", "issue_comment 750 withdraws 0"}
	if !slices.Equal(got, want) {
		t.Fatalf("intake events = %q, want %q", got, want)
	}
	answers, _, _ := answersFrom(events)
	if want := []string{`sebastian (2026-10-15), to "The gate supervisor stopped ModelReview (attempt 1) past its bound of 10m0s: hangs": gateway restarted`}; !slices.Equal(answers, want) {
		t.Fatalf("answers = %q, want %q", answers, want)
	}
}

// A factory_closed row keeps its comment and waits for its confirmation:
// listed once older than the bound, oldest first, until confirmed; a
// confirmation is final, and no other kind of row is ever confirmed.
func TestFactoryCloseConfirmation(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, intake_events CASCADE`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, Task{ID: "t-fc", Status: "in-review", Objective: "o", Repository: "sf/r", Kind: "agent", IssueNumber: 9}); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, e := range []IntakeEvent{
		{TaskID: "t-fc", Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: 9, Body: "closing: done"},
		{TaskID: "t-fc", Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: 10, Body: "closing: also done"},
		{TaskID: "t-fc", Kind: "issue_comment", Author: "sebastian", CommentID: 11, Body: "hello"},
	} {
		id, err := s.InsertIntakeEvent(ctx, e)
		if err != nil || id == 0 {
			t.Fatalf("insert %s: id %d, err %v", e.Kind, id, err)
		}
		ids = append(ids, id)
	}
	got, err := s.UnconfirmedFactoryCloses(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != ids[0] || got[1].ID != ids[1] {
		t.Fatalf("unconfirmed = %+v, want rows %d then %d", got, ids[0], ids[1])
	}
	if e := got[0]; e.TaskID != "t-fc" || e.Kind != IntakeFactoryClosed || e.CommentID != 9 || e.Body != "closing: done" || e.CreatedAt.IsZero() {
		t.Fatalf("unconfirmed[0] = %+v", e)
	}
	if got, err := s.UnconfirmedFactoryCloses(ctx, 2*time.Minute, 10); err != nil || len(got) != 0 {
		t.Fatalf("a row made now, olderThan 2m: %+v, %v", got, err)
	}
	if got, err := s.UnconfirmedFactoryCloses(ctx, 0, 1); err != nil || len(got) != 1 || got[0].ID != ids[0] {
		t.Fatalf("limit 1: %+v, %v", got, err)
	}
	if err := s.ConfirmFactoryClose(ctx, "t-fc", 9); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmFactoryClose(ctx, "t-fc", 9); err != nil {
		t.Fatalf("second confirm: %v", err)
	}
	if err := s.ConfirmFactoryClose(ctx, "t-fc", 11); err != nil {
		t.Fatalf("confirm of a comment row: %v", err)
	}
	if err := s.ConfirmFactoryClose(ctx, "t-other", 10); err != nil {
		t.Fatalf("confirm of another task's issue: %v", err)
	}
	if err := s.ConfirmFactoryClose(ctx, "t-fc", 99); err != nil {
		t.Fatalf("confirm of a missing row: %v", err)
	}
	got, err = s.UnconfirmedFactoryCloses(ctx, 0, 10)
	if err != nil || len(got) != 1 || got[0].ID != ids[1] {
		t.Fatalf("after confirming %d: %+v, %v", ids[0], got, err)
	}
	var confirmed int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM intake_events WHERE confirmed_at IS NOT NULL`).Scan(&confirmed); err != nil || confirmed != 1 {
		t.Fatalf("confirmed rows = %d, err %v, want 1", confirmed, err)
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

// NewestAlertRows keeps each fingerprint's newest row of the kind, and that
// only when it names the namespace, oldest first: a fingerprint whose
// newest row names none is not listed by an older row that does. "" reads
// every namespace's.
func TestPGStoreNewestAlertRows(t *testing.T) {
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
	insert := func(kind, fingerprint, namespace string) int64 {
		t.Helper()
		detail := map[string]any{"starts_at": "2026-10-15T09:00:00Z"}
		if namespace != "" {
			detail["namespace"] = namespace
		}
		id, err := s.InsertAlert(ctx, Alert{Kind: kind, Source: "r", Fingerprint: fingerprint, Detail: detail})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	insert(AlertSigNoz, "fp1", "demo-prod")
	fp1 := insert(AlertSigNoz, "fp1", "demo-prod")
	flux := insert(AlertSigNoz, "flux/demo-prod/demo-app", "demo-prod")
	insert(AlertSigNoz, "fp3", "demo-prod")
	fp3 := insert(AlertSigNoz, "fp3", "")
	fp4 := insert(AlertSigNoz, "fp4", "demo-staging")
	insert(AlertEscalation, "fp5", "demo-prod")
	got, err := s.NewestAlertRows(ctx, AlertSigNoz, "demo-prod")
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, a := range got {
		ids = append(ids, a.ID)
	}
	if want := []int64{fp1, flux}; !slices.Equal(ids, want) || got[1].Detail["starts_at"] != "2026-10-15T09:00:00Z" {
		t.Fatalf("rows %+v, want ids %v", got, want)
	}
	every, err := s.NewestAlertRows(ctx, AlertSigNoz, "")
	if err != nil {
		t.Fatal(err)
	}
	ids = nil
	for _, a := range every {
		ids = append(ids, a.ID)
	}
	if want := []int64{fp1, flux, fp3, fp4}; !slices.Equal(ids, want) {
		t.Fatalf("every namespace's rows %v, want %v", ids, want)
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
	// a later answer's suspect verdict opens its own once the earlier one
	// is judged (item 132), and a retry still finds it
	if id, err := s.InsertAlert(ctx, Alert{Kind: AlertScreening, Source: "outside account", TaskID: "t-parent"}); err != nil || id == 0 {
		t.Fatalf("screening alert after the judged one: id %d, %v", id, err)
	}
	if id, err := s.InsertAlert(ctx, Alert{Kind: AlertScreening, Source: "outside account", TaskID: "t-parent"}); err != nil || id != 0 {
		t.Fatalf("second open screening alert: id %d, %v", id, err)
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
		INSERT INTO ledger (task_id, kind, ref, eur) VALUES ('same', 'model', 'small', 0.25)`); err != nil {
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
	if r, err := s.CapRefusal(ctx, "t", time.Date(2026, 10, 1, 0, 35, 0, 0, time.UTC)); err != nil || r == nil || r.Refusal != (policy.Refusal{Decision: policy.RefusedMonthCap, Reason: "later"}) {
		t.Fatalf("t's refusal since 00:35 = (%+v, %v), want later", r, err)
	}
	// the month's cap before the day's, which resets on its own
	if r, err := s.CapRefusal(ctx, "t", time.Date(2026, 10, 1, 0, 15, 0, 0, time.UTC)); err != nil || r == nil || r.Refusal != (policy.Refusal{Decision: policy.RefusedMonthCap, Reason: "later"}) {
		t.Fatalf("t's refusal since 00:15 = (%+v, %v), want later", r, err)
	}
	if r, err := s.CapRefusal(ctx, "t", time.Date(2026, 10, 1, 0, 45, 0, 0, time.UTC)); err != nil || r != nil {
		t.Fatalf("t's refusal since 00:45 = %+v, %v, want none", r, err)
	}
	// a reservation's worst case is refused while model calls still fit:
	// no session is capped by it
	if r, err := s.CapRefusal(ctx, "r", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); err != nil || r != nil {
		t.Fatalf("r's refusal = (%+v, %v), want none: a tool call's", r, err)
	}
	if r, err := s.CapRefusal(ctx, "v", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); err != nil || r != nil {
		t.Fatalf("v's refusal = %+v, %v, want none", r, err)
	}
	// the project's cap is read beside the month's, the newer of the two,
	// and before a newer day cap's
	if _, err := s.db.ExecContext(ctx, `INSERT INTO audit (task_id, decision, reason, ts) VALUES
		('p', 'refused_month_cap', 'the month', '2026-10-01 00:50:00Z'), ('p', 'refused_project_cap', 'the project', '2026-10-01 00:55:00Z'),
		('p', 'refused_budget', 'the task', '2026-10-01 00:58:00Z'), ('p', 'refused_day_cap', 'the day', '2026-10-01 00:59:00Z')`); err != nil {
		t.Fatal(err)
	}
	if r, err := s.CapRefusal(ctx, "p", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); err != nil || r == nil || r.Refusal != (policy.Refusal{Decision: policy.RefusedProjectCap, Reason: "the project"}) {
		t.Fatalf("p's refusal = (%+v, %v), want the project's", r, err)
	}
	// a task the day cap alone refused: its newest, with its time, whose
	// UTC day turns at 2026-10-02 00:00 UTC though the session's zone is
	// still on 2026-10-01 16:20
	if _, err := s.db.ExecContext(ctx, `INSERT INTO audit (task_id, decision, reason, ts) VALUES
		('d', 'refused_day_cap', 'first', '2026-10-01 23:10:00Z'), ('d', 'refused_day_cap', 'second', '2026-10-01 23:20:00Z')`); err != nil {
		t.Fatal(err)
	}
	r, err := s.CapRefusal(ctx, "d", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || r == nil || r.Refusal != (policy.Refusal{Decision: policy.RefusedDayCap, Reason: "second"}) || !r.At.Equal(time.Date(2026, 10, 1, 23, 20, 0, 0, time.UTC)) || r.Allowed {
		t.Fatalf("d's refusal = (%+v, %v), want the day cap's second at 23:20 UTC, no call allowed", r, err)
	}
	if got := dayTurn(r.At); !got.Equal(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("the day of %v turns at %v, want 2026-10-02 00:00 UTC", r.At, got)
	}
	// an agent's call allowed before the refusal: the session made calls;
	// a supervisor's allowed call is no session's
	if _, err := s.db.ExecContext(ctx, `INSERT INTO audit (task_id, decision, reason, kind, ts) VALUES
		('e', 'allowed', '', 'supervisor', '2026-10-01 22:00:00Z'), ('e', 'refused_day_cap', 'the day', 'agent', '2026-10-01 22:10:00Z'),
		('f', 'allowed', '', 'agent', '2026-10-01 22:00:00Z'), ('f', 'refused_day_cap', 'the day', 'agent', '2026-10-01 22:10:00Z')`); err != nil {
		t.Fatal(err)
	}
	for id, allowed := range map[string]bool{"e": false, "f": true} {
		if r, err := s.CapRefusal(ctx, id, time.Date(2026, 10, 1, 21, 0, 0, 0, time.UTC)); err != nil || r == nil || r.Allowed != allowed {
			t.Fatalf("%s's refusal = (%+v, %v), want allowed %v", id, r, err, allowed)
		}
	}
	// a tool call refused at the day cap beside model calls that fit is no
	// cap of the session's
	if _, err := s.db.ExecContext(ctx, `INSERT INTO audit (task_id, decision, reason, tool, ts) VALUES
		('g', 'allowed', '', '', '2026-10-01 22:00:00Z'), ('g', 'refused_day_cap', 'web search worst case', 'web_search', '2026-10-01 22:05:00Z'),
		('g', 'allowed', '', '', '2026-10-01 22:10:00Z')`); err != nil {
		t.Fatal(err)
	}
	if r, err := s.CapRefusal(ctx, "g", time.Date(2026, 10, 1, 21, 0, 0, 0, time.UTC)); err != nil || r != nil {
		t.Fatalf("g's refusal = (%+v, %v), want none: only a tool call was refused", r, err)
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

// A session got a model answer when the gateway booked one of its agent's
// calls allowed: a supervisor's call, a refused call and an earlier session's
// do not count.
func TestPGStoreModelAnswered(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_answered"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE audit;
		INSERT INTO audit (task_id, decision, reason, kind, ts) VALUES
		('a', 'allowed', '', 'supervisor', '2026-10-15 10:00:00Z'),
		('b', 'refused_budget', 'the task', 'agent', '2026-10-15 10:00:00Z'),
		('c', 'allowed', '', 'agent', '2026-10-15 10:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) time.Time { return time.Date(2026, 10, 15, h, m, 0, 0, time.UTC) }
	for _, tc := range []struct {
		task  string
		since time.Time
		want  bool
	}{
		{"a", at(9, 0), false}, {"b", at(9, 0), false}, {"c", at(9, 0), true}, {"c", at(10, 0), true}, {"c", at(10, 1), false}, {"nobody", at(9, 0), false},
	} {
		if got, err := s.ModelAnswered(ctx, tc.task, tc.since); err != nil || got != tc.want {
			t.Errorf("ModelAnswered(%s, %v) = (%v, %v), want %v", tc.task, tc.since, got, err, tc.want)
		}
	}
}

// The check-in's spend and calls on a card: every purpose checkin row of
// the task counts its calls, a tool's call among the calls but costing
// nothing (the gateway books the task's running spend into spend_eur and
// the call's own cost into call_cost_eur, so the share counts only
// call_cost_eur); other kinds and other tasks count not. Hand-derived:
// 0.02 + 0 + 0.03 = 0.05, not the running totals 0.20 + 0.20 + 0.25.
func TestCheckinSpentAndCalls(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_checkin"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE audit;
		INSERT INTO audit (task_id, decision, reason, kind, tool, spend_eur, call_cost_eur, ts) VALUES
		('a', 'allowed', '', 'checkin', '', 0.20, 0.02, '2026-10-17 03:52:00Z'),
		('a', 'allowed', '', 'checkin', 'run', 0.20, NULL, '2026-10-17 03:52:04Z'),
		('a', 'allowed', '', 'checkin', '', 0.25, 0.03, '2026-10-17 04:20:00Z'),
		('a', 'allowed', '', 'agent', '', 0.60, 0.35, '2026-10-17 04:21:00Z'),
		('b', 'allowed', '', 'checkin', '', 0.04, 0.04, '2026-10-17 04:22:00Z')`); err != nil {
		t.Fatal(err)
	}
	eur, err := s.CheckinSpent(ctx, "a")
	if err != nil || eur != 0.05 {
		t.Errorf("CheckinSpent(a) = (%v, %v), want (0.05, nil)", eur, err)
	}
	n, err := s.CheckinCalls(ctx, "a")
	if err != nil || n != 3 {
		t.Errorf("CheckinCalls(a) = (%v, %v), want (3, nil)", n, err)
	}
}

// A provider refusal is the first model call an agent made since the
// session began refused upstream, with no call of it allowed since: a call
// before the session, a tool's call and factoryd's own calls count not.
func TestPGStoreProviderRefusal(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE audit;
		INSERT INTO audit (task_id, decision, reason, tool, kind, ts) VALUES
			('a', 'error_upstream', 'upstream 429', '', 'agent', '2026-10-11 05:00:01Z'),
			('a', 'error_upstream', 'upstream 429', '', 'agent', '2026-10-11 05:00:02Z'),
			('b', 'allowed', '', '', 'agent', '2026-10-11 05:00:01Z'),
			('b', 'error_upstream', 'upstream 429', '', 'agent', '2026-10-11 05:00:02Z'),
			('d', 'refused_budget', 'the task', '', 'agent', '2026-10-11 05:00:01Z'),
			('d', 'error_upstream', 'upstream 503', '', 'agent', '2026-10-11 05:00:02Z'),
			('e', 'allowed', '', '', 'agent', '2026-10-11 04:59:59Z'),
			('e', 'error_upstream', 'upstream 503', '', 'agent', '2026-10-11 05:00:01Z'),
			('e', 'allowed', '', 'web_search', 'agent', '2026-10-11 05:00:02Z'),
			('e', 'allowed', '', '', 'supervisor', '2026-10-11 05:00:03Z'),
			('f', 'error_upstream', 'upstream 429', '', 'agent', '2026-10-11 05:00:01Z'),
			('f', 'allowed', '', '', 'agent', '2026-10-11 05:00:02Z')`); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 10, 11, 5, 0, 0, 0, time.UTC)
	for task, want := range map[string]string{"a": "upstream 429", "b": "", "c": "", "d": "", "e": "upstream 503", "f": ""} {
		reason, ok, err := s.ProviderRefusal(ctx, task, since)
		if err != nil || ok != (want != "") || reason != want {
			t.Errorf("ProviderRefusal(%s) = %q, %v, %v; want %q", task, reason, ok, err, want)
		}
	}
}

func (s *fakeStore) PutPlanQuestions(ctx context.Context, id string, revision int, qs []PlanQuestion) (bool, error) {
	k := fmt.Sprintf("%s %d", id, revision)
	if _, ok := s.questions[k]; ok {
		return false, nil
	}
	if s.questions == nil {
		s.questions = map[string][]PlanQuestion{}
		s.kinds = map[string][]string{}
	}
	s.questions[k] = slices.Clone(qs)
	s.kinds[k] = make([]string, len(qs))
	for i := range s.kinds[k] {
		s.kinds[k][i] = CatchQuestion
	}
	return true, nil
}

func (s *fakeStore) PutPlanConfirmation(ctx context.Context, id string, revision int, q PlanQuestion) (int, bool, error) {
	k := fmt.Sprintf("%s %d", id, revision)
	if slices.Contains(s.kinds[k], "confirmation") {
		return 0, false, nil
	}
	if s.questions == nil {
		s.questions = map[string][]PlanQuestion{}
		s.kinds = map[string][]string{}
	}
	s.questions[k] = append(s.questions[k], q)
	s.kinds[k] = append(s.kinds[k], "confirmation")
	return len(s.questions[k]), true, nil
}

func (s *fakeStore) PlanConfirmationAnswered(ctx context.Context, id string) (bool, error) {
	for k, kinds := range s.kinds {
		if !strings.HasPrefix(k, id+" ") {
			continue
		}
		for i, kind := range kinds {
			if kind == "confirmation" && s.questions[k][i].Answer != "" {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *fakeStore) PutPlanCatches(ctx context.Context, id string, revision int, kind, finder string, details []string) (bool, error) {
	k := fmt.Sprintf("%s %d %s %s", id, revision, kind, finder)
	if _, ok := s.catches[k]; ok {
		return false, nil
	}
	if s.catches == nil {
		s.catches = map[string][]string{}
	}
	s.catches[k] = slices.Clone(details)
	return true, nil
}

func (s *fakeStore) PutCriteriaVerdicts(ctx context.Context, taskID, source, by string, rows []CriterionVerdict) error {
	if s.criteriaVerdicts == nil {
		s.criteriaVerdicts = map[string][]CriterionVerdict{}
	}
	replaced := map[string]bool{}
	for _, v := range rows {
		replaced[v.Criterion] = true
	}
	var out []CriterionVerdict
	for _, v := range s.criteriaVerdicts[taskID] {
		if v.Source != source || !replaced[v.Criterion] {
			out = append(out, v)
		}
	}
	for _, v := range rows {
		v.Source, v.DecidedBy = source, by
		out = append(out, v)
	}
	s.criteriaVerdicts[taskID] = out
	return nil
}

func (s *fakeStore) CriteriaVerdicts(ctx context.Context, taskID string) ([]CriterionVerdict, error) {
	return s.criteriaVerdicts[taskID], nil
}

func (s *fakeStore) CriteriaVerdictTasks(ctx context.Context) ([]string, error) {
	var ids []string
	for id := range s.criteriaVerdicts {
		ids = append(ids, id)
	}
	return ids, nil
}

func (s *fakeStore) PlanCatches(ctx context.Context, id string) ([]PlanCatch, error) {
	var out []PlanCatch
	for k, details := range s.catches {
		var cID string
		var rev int
		var kind, finder string
		if _, err := fmt.Sscanf(k, "%s %d %s %s", &cID, &rev, &kind, &finder); err != nil || cID != id {
			continue
		}
		for _, d := range details {
			out = append(out, PlanCatch{TaskID: id, Revision: rev, Kind: kind, Finder: finder, Detail: d})
		}
	}
	return out, nil
}

func (s *fakeStore) PlanQuestions(ctx context.Context, id string, revision int) ([]PlanQuestion, error) {
	return slices.Clone(s.questions[fmt.Sprintf("%s %d", id, revision)]), nil
}

func (s *fakeStore) AnswerPlanQuestion(ctx context.Context, id string, revision, idx int, answer, by string) error {
	qs := s.questions[fmt.Sprintf("%s %d", id, revision)]
	switch {
	case idx < 1 || idx > len(qs):
		return ErrNotFound
	case qs[idx-1].Answer != "":
		return ErrAnswered
	}
	qs[idx-1].Answer, qs[idx-1].AnsweredBy, qs[idx-1].AnsweredAt = answer, by, time.Now()
	return nil
}

func (s *fakeStore) AnswerPlanQuestions(ctx context.Context, id string, revision int, answers []PlanAnswer, by string) error {
	qs := s.questions[fmt.Sprintf("%s %d", id, revision)]
	for _, a := range answers {
		switch {
		case a.Idx < 1 || a.Idx > len(qs):
			return ErrNotFound
		case qs[a.Idx-1].Answer != "":
			return ErrAnswered
		}
	}
	for _, a := range answers {
		qs[a.Idx-1].Answer, qs[a.Idx-1].AnsweredBy, qs[a.Idx-1].AnsweredAt = a.Text, by, time.Now()
	}
	return nil
}

func (s *fakeStore) OpenPlanQuestions(ctx context.Context, id string, revision int) (int, error) {
	qs, _ := s.PlanQuestions(ctx, id, revision)
	return len(qs) - len(slices.DeleteFunc(qs, func(q PlanQuestion) bool { return q.Answer == "" })), nil
}

func (s *fakeStore) ConfirmationIndex(ctx context.Context, id string, revision int) (int, bool, error) {
	for i, kind := range s.kinds[fmt.Sprintf("%s %d", id, revision)] {
		if kind == QuestionConfirmation {
			return i + 1, true, nil
		}
	}
	return 0, false, nil
}

func (s *fakeStore) PutPlanHoldouts(ctx context.Context, id string, round int, drafts []Holdout) (bool, error) {
	for _, h := range s.holdouts[id] {
		if h.Round == round {
			return false, nil
		}
	}
	if s.holdouts == nil {
		s.holdouts = map[string][]Holdout{}
	}
	for _, h := range drafts {
		h.Round = round
		s.holdouts[id] = append(s.holdouts[id], h)
	}
	return len(drafts) > 0, nil
}

func (s *fakeStore) PlanHoldouts(ctx context.Context, id string) ([]Holdout, error) {
	return slices.Clone(s.holdouts[id]), nil
}

func (s *fakeStore) UndecidedHoldouts(ctx context.Context, id string, round int) (int, error) {
	n := 0
	for _, h := range s.holdouts[id] {
		if h.Round == round && (h.State == HoldoutProposed || h.State == HoldoutUnrunnable) {
			n++
		}
	}
	return n, nil
}

func (s *fakeStore) DecidePlanHoldout(ctx context.Context, id string, round, idx int, state string) error {
	for i, h := range s.holdouts[id] {
		if h.Round != round || h.Idx != idx {
			continue
		}
		takes := (state == HoldoutDropped && (h.State == HoldoutProposed || h.State == HoldoutUnrunnable)) ||
			(state == HoldoutStored && h.State == HoldoutProposed && (h.DryVerdict == "red" || h.DryVerdict == "green"))
		if !takes {
			return ErrHoldoutClick
		}
		s.holdouts[id][i].State = state
		return nil
	}
	for _, h := range s.holdouts[id] {
		if h.Idx == idx {
			return ErrHoldoutClick
		}
	}
	return ErrNoHoldout
}

func (s *fakeStore) SweepPlanHoldouts(ctx context.Context, id string) error {
	for i, h := range s.holdouts[id] {
		if h.State == HoldoutProposed || h.State == HoldoutUnrunnable {
			s.holdouts[id][i].State = HoldoutDropped
		}
	}
	return nil
}

func (s *fakeStore) SetHoldoutAudit(ctx context.Context, id string, idx int, verdict, reason, rewrite string) error {
	for i, h := range s.holdouts[id] {
		if h.Idx != idx {
			continue
		}
		if h.AuditVerdict != "" || (h.State != HoldoutStored && h.State != HoldoutOpened) {
			return ErrAnswered
		}
		s.holdouts[id][i].AuditVerdict, s.holdouts[id][i].AuditReason, s.holdouts[id][i].Rewrite = verdict, reason, rewrite
		if verdict == AuditWrongTest && rewrite == "" {
			s.holdouts[id][i].State = HoldoutDropped
		}
		return nil
	}
	return ErrNoHoldout
}

func (s *fakeStore) ClearHoldoutAudit(ctx context.Context, id string, idx int) error {
	for i, h := range s.holdouts[id] {
		if h.Idx == idx {
			s.holdouts[id][i].AuditVerdict, s.holdouts[id][i].AuditReason, s.holdouts[id][i].Rewrite = "", "", ""
		}
	}
	return nil
}

func (s *fakeStore) SetHoldoutTest(ctx context.Context, id string, idx int, code, verdict, output string) error {
	for i, h := range s.holdouts[id] {
		if h.Idx == idx {
			s.holdouts[id][i].TestCode, s.holdouts[id][i].DryVerdict, s.holdouts[id][i].DryOutput = code, verdict, output
			s.holdouts[id][i].AuditVerdict, s.holdouts[id][i].AuditReason, s.holdouts[id][i].Rewrite = "", "", ""
		}
	}
	return nil
}

func (s *fakeStore) BumpHoldoutRed(ctx context.Context, id string, idx int) (int, error) {
	for i := range s.holdouts[id] {
		if s.holdouts[id][i].Idx == idx {
			s.holdouts[id][i].RedCycles++
			return s.holdouts[id][i].RedCycles, nil
		}
	}
	return 0, ErrNoHoldout
}

func (s *fakeStore) OpenHoldout(ctx context.Context, id string, idx int) (string, error) {
	for i, h := range s.holdouts[id] {
		if h.Idx != idx {
			continue
		}
		if h.State != HoldoutStored && h.State != HoldoutOpened {
			return "", ErrNoHoldout
		}
		s.holdouts[id][i].State = HoldoutOpened
		return h.ID, nil
	}
	return "", ErrNoHoldout
}

func (s *fakeStore) PastReverts(ctx context.Context, repository string) ([]string, error) {
	var reverts []Task
	for _, t := range s.tasks {
		if t.Repository == repository && t.Source == SourceRevert {
			reverts = append(reverts, t)
		}
	}
	slices.SortFunc(reverts, func(a, b Task) int { return b.CreatedAt.Compare(a.CreatedAt) })
	var out []string
	for _, t := range reverts[:min(3, len(reverts))] {
		if line, _, _ := strings.Cut(t.Objective, "\n"); strings.TrimSpace(line) != "" {
			out = append(out, textutil.Truncate(strings.TrimSpace(line), maxHoldoutText))
		}
	}
	return out, nil
}

// RaiseTaskBudget (roadmap 210: the console's Fund action) raises
// budget_eur by the stated amount in one statement and answers the budget
// before and after; a missing task is ErrNotFound.
func TestPGStoreRaiseTaskBudget(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks CASCADE`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, Task{ID: "t-c", BudgetEUR: 2, SpentEUR: 1.83, Status: "waiting-human", Repository: "sf/r"}); err != nil {
		t.Fatal(err)
	}
	from, to, err := s.RaiseTaskBudget(ctx, "t-c", 0.5)
	if err != nil || from != 2 || to != 2.5 {
		t.Fatalf("raise = (%v, %v, %v), want (2, 2.5, nil)", from, to, err)
	}
	if got, _, _ := s.Task(ctx, "t-c"); got.BudgetEUR != 2.5 || got.SpentEUR != 1.83 {
		t.Fatalf("after the raise: budget %v spent %v, want 2.5 and 1.83", got.BudgetEUR, got.SpentEUR)
	}
	if _, _, err := s.RaiseTaskBudget(ctx, "nope", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("raise of no task = %v, want ErrNotFound", err)
	}
}

func (s *fakeStore) OverHoldCall(ctx context.Context, taskID string, since time.Time) (*OverHoldCall, error) {
	if c, ok := s.overHolds[taskID]; ok {
		return &c, nil
	}
	return nil, nil
}

// BudgetRefused on a real Postgres: only an agent's model call refused at
// the task's own budget since since counts — not one before it, not one a
// tool's worst case caused, not one the supervisor got.
func TestPGStoreBudgetRefused(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE audit;
		INSERT INTO audit (task_id, kind, decision, reason, tool, ts) VALUES
			('a', 'agent', 'refused_budget', 'before the session', '', '2026-10-15 10:00:00Z'),
			('b', 'agent', 'refused_budget', 'a tool', 'web_scrape', '2026-10-15 11:30:00Z'),
			('c', 'supervisor', 'refused_budget', 'factoryd call', '', '2026-10-15 11:30:00Z'),
			('d', 'agent', 'refused_budget', 'in the session', '', '2026-10-15 11:30:00Z')`); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 10, 15, 11, 0, 0, 0, time.UTC)
	for task, want := range map[string]bool{"a": false, "b": false, "c": false, "d": true, "none": false} {
		if got, err := s.BudgetRefused(ctx, task, since); err != nil || got != want {
			t.Errorf("BudgetRefused(%s) = (%v, %v), want %v", task, got, err, want)
		}
	}
}

// TaskStatuses reads many cards' statuses at once: a card with no row no
// entry.
func TestTaskStatuses(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_statuses"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, finished_at) VALUES
		('t-wait', 1, 0, 'waiting-human', 'sf/r', 'agent', 'webhook', NULL),
		('t-run', 1, 0, 'running', 'sf/r', 'agent', 'webhook', NULL),
		('t-done', 1, 0, 'done', 'sf/r', 'agent', 'webhook', now())`); err != nil {
		t.Fatal(err)
	}
	got, err := s.TaskStatuses(ctx, []string{"t-wait", "t-run", "t-missing"})
	if err != nil {
		t.Fatalf("TaskStatuses: %v", err)
	}
	if len(got) != 2 || got["t-wait"] != "waiting-human" || got["t-run"] != "running" {
		t.Fatalf("TaskStatuses = %v, want the two rows' statuses", got)
	}
	if empty, err := s.TaskStatuses(ctx, nil); err != nil || len(empty) != 0 {
		t.Fatalf("TaskStatuses of none = %v, %v, want empty", empty, err)
	}
}

// One card per issue (roadmap 298): the store refuses a second task still
// live on the same issue number of the same repository, naming the card
// that holds it; a final one frees the issue, and another repository's
// same number is another issue. Identity only — likeness never judged.
func TestPGStoreOneCardPerIssue(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks CASCADE`); err != nil {
		t.Fatal(err)
	}
	const repo = "sf/ai-software-factory-code"
	file := func(id string, issue int64, repo string) error {
		return s.CreateTask(ctx, Task{ID: id, Status: "queued", Repository: repo, Kind: "agent", Source: "webhook", IssueNumber: issue})
	}
	if err := file("t-first", 1109, repo); err != nil {
		t.Fatal(err)
	}
	err = file("t-second", 1109, repo)
	if err == nil || !strings.Contains(err.Error(), "t-first already holds issue 1109") {
		t.Fatalf("a second live card on issue 1109: %v", err)
	}
	if err := file("t-other-repo", 1109, "sf/another-repo"); err != nil {
		t.Fatalf("another repository's issue 1109: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'done', finished_at = now() WHERE id = 't-first'`); err != nil {
		t.Fatal(err)
	}
	if err := file("t-second", 1109, repo); err != nil {
		t.Fatalf("a done card frees its issue: %v", err)
	}
}

// justNow is a merged fixture's finish stamp, as the database gives every
// merged row.
func justNow() *time.Time {
	now := time.Now()
	return &now
}
