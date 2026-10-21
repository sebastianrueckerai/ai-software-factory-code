package acceptance

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// stubForgejo is demo-app's repository as Forgejo answers it: issues filed
// and labelled, comments posted, the task branch and its pull request. The
// card's workflow reads its state through the same server the intake filed
// on, so no step of the chain is stubbed away.
type stubForgejo struct {
	mu        sync.Mutex
	base      string // the httptest server's URL; set by server
	issues    []string
	num       int64
	labels    map[int64][]string
	comments  []string
	closedPRs []int64
	tokens    []factoryd.TokenRef
	nextID    int64
}

func (f *stubForgejo) baseURL() string { return f.base }

func (f *stubForgejo) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		repo := "/api/v1/repos/demo-app/demo-app"
		fork := "/api/v1/repos/factory-agent/demo-app"
		var n int64
		switch {
		case r.Method == http.MethodGet && r.URL.Path == repo:
			io.WriteString(w, `{"default_branch":"main"}`)
		case r.Method == http.MethodGet && r.URL.Path == repo+"/branches/main":
			io.WriteString(w, `{"commit":{"id":"sha-demo-main"}}`)
		case r.Method == http.MethodPost && r.URL.Path == repo+"/issues":
			var in struct{ Title, Body string }
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f.num++
			f.issues = append(f.issues, in.Title+"\n\n"+in.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"number":%d,"html_url":"https://git.example/demo-app/demo-app/issues/%d"}`, f.num, f.num)
		case r.Method == http.MethodPut && fmtScan(r.URL.Path, repo+"/issues/%d/labels", &n):
			var in struct{ Labels []string }
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if f.labels == nil {
				f.labels = map[int64][]string{}
			}
			f.labels[n] = in.Labels
			io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && fmtScan(r.URL.Path, repo+"/issues/%d/labels", &n):
			out := []map[string]string{}
			for _, l := range f.labels[n] {
				out = append(out, map[string]string{"name": l})
			}
			if err := json.NewEncoder(w).Encode(out); err != nil {
				t.Errorf("labels answer: %v", err)
			}
		case r.Method == http.MethodPost && fmtScan(r.URL.Path, repo+"/issues/%d/comments", &n):
			var in struct{ Body string }
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f.comments = append(f.comments, fmt.Sprintf("#%d %s", n, in.Body))
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":1}`)
		case r.Method == http.MethodGet && fmtScan(r.URL.Path, repo+"/issues/%d", &n):
			io.WriteString(w, `{"state":"open"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, fork+"/branches/"):
			io.WriteString(w, `{"commit":{"id":"c-branch"}}`)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, fork+"/branches/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == repo+"/pulls":
			io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, repo+"/pulls/"):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/users/factory-agent/tokens"):
			list, _ := json.Marshal(f.tokens)
			w.Write(list) // #nosec G104 -- a test stub's answer
		case r.Method == http.MethodDelete && fmtScan(r.URL.Path, "/api/v1/users/factory-agent/tokens/%d", &n):
			f.tokens = slices.DeleteFunc(f.tokens, func(tok factoryd.TokenRef) bool { return tok.ID == n })
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	f.base = srv.URL
	return srv
}

func fmtScan(path, format string, n *int64) bool {
	c, err := fmt.Sscanf(path, format, n)
	return err == nil && c == 1 && fmt.Sprintf(format, *n) == path
}

// stubObjects is the timeline's object store in memory.
type stubObjects struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (o *stubObjects) Put(_ context.Context, key string, body []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.data == nil {
		o.data = map[string][]byte{}
	}
	o.data[key] = slices.Clone(body)
	return nil
}

func (o *stubObjects) Get(_ context.Context, key string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.data[key]), nil
}

func (o *stubObjects) Delete(_ context.Context, key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.data, key)
	return nil
}

// acceptanceStore is the task and alert rows behind both the intake and
// the card's workflow: the same Store interface the shipped PGStore
// answers, so every activity runs its real code.
type acceptanceStore struct {
	mu     sync.Mutex
	tasks  map[string]factoryd.Task
	alerts []factoryd.Alert
	events []factoryd.IntakeEvent
	// confirmedCloses are the factory_closed rows ConfirmFactoryClose marked, by task and issue.
	confirmedCloses map[[2]any]bool
	sessions        []factoryd.Session
	queue           []string // the merge queue, oldest first
	finals          [][2]string
	deps            map[string][]string
	approvals       []factoryd.ApprovalRequest
}

func newAcceptanceStore() *acceptanceStore {
	return &acceptanceStore{tasks: map[string]factoryd.Task{}, deps: map[string][]string{}}
}

func (s *acceptanceStore) CreateTask(_ context.Context, t factoryd.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.Kind == "" {
		t.Kind = "agent"
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	if t.Kind == "agent" && t.Status != "backlog" && t.Gate == "" {
		t.Gate, t.GateSince = factoryd.GateIntake, &t.CreatedAt
	}
	s.tasks[t.ID] = t
	return nil
}

func (s *acceptanceStore) Task(_ context.Context, id string) (factoryd.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	return t, ok, nil
}

func (s *acceptanceStore) TransitionTask(_ context.Context, id string, from []string, to string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok || !slices.Contains(from, t.Status) {
		return fmt.Errorf("task %s: %w (not in %v, cannot move to %s)", id, factoryd.ErrNotFound, from, to)
	}
	t.Status = to
	if factoryd.Final(to) {
		now := time.Now()
		t.FinishedAt, t.Gate, t.GateSince = &now, "", nil
	}
	s.tasks[id] = t
	return nil
}

func (s *acceptanceStore) MarkStarted(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok || !slices.Contains([]string{"queued", "waiting-deps", "running", "in-review", "waiting-human", "waiting-poster"}, t.Status) {
		return fmt.Errorf("task %s: %w", id, factoryd.ErrNotFound)
	}
	t.Status = "running"
	s.tasks[id] = t
	return nil
}

func (s *acceptanceStore) SetAdmitted(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %s: %w", id, factoryd.ErrNotFound)
	}
	if t.AdmittedAt == nil {
		now := time.Now()
		t.AdmittedAt = &now
		s.tasks[id] = t
	}
	return nil
}

func (s *acceptanceStore) CountAdmitted(_ context.Context, repository string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.tasks {
		if t.Repository != repository || t.Kind != "agent" || t.Lane == "" {
			continue
		}
		if slices.Contains([]string{"waiting-deps", "running", "in-review", "waiting-human", "waiting-poster", "closing"}, t.Status) ||
			(t.Status == "queued" && t.AdmittedAt != nil) {
			n++
		}
	}
	return n, nil
}

func (s *acceptanceStore) ListQueuedByRepository(_ context.Context, repository string) ([]factoryd.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []factoryd.Task
	for _, t := range s.tasks {
		if t.Repository == repository && t.Kind == "agent" && t.Lane != "" && t.Status == "queued" {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b factoryd.Task) int {
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

func (s *acceptanceStore) ListTasksByStatus(_ context.Context, status string) ([]factoryd.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []factoryd.Task
	for _, t := range s.tasks {
		if t.Status == status {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *acceptanceStore) RecentlyFinishedTasks(context.Context, time.Duration) ([]string, error) {
	return nil, nil
}

func (s *acceptanceStore) SettleSpend(_ context.Context, id string, reported float64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return false, fmt.Errorf("task %s: not found", id)
	}
	changed := reported > t.SpentEUR
	t.SpentEUR = max(t.SpentEUR, reported)
	s.tasks[id] = t
	return changed, nil
}

func (s *acceptanceStore) UnsettledTasks(context.Context, time.Duration) ([]string, error) {
	return nil, nil
}

func (s *acceptanceStore) SettleSpendFinal(_ context.Context, id string, reported float64) (bool, error) {
	return s.SettleSpend(context.Background(), id, reported)
}

func (s *acceptanceStore) TaskByPR(_ context.Context, repository string, number int64) (factoryd.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tasks {
		if t.Repository == repository && t.PRNumber == number && number != 0 {
			return t, true, nil
		}
	}
	return factoryd.Task{}, false, nil
}

// TasksByPR lists every card linked to number on repository, newest first,
// as the SQL store does: a webhook on a factory pull request picks the
// open card among them.
func (s *acceptanceStore) TasksByPR(_ context.Context, repository string, number int64) ([]factoryd.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cards []factoryd.Task
	for _, t := range s.tasks {
		if t.Repository == repository && t.PRNumber == number && number != 0 {
			cards = append(cards, t)
		}
	}
	slices.SortFunc(cards, func(a, b factoryd.Task) int {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return b.CreatedAt.Compare(a.CreatedAt)
		}
		return strings.Compare(b.ID, a.ID)
	})
	return cards, nil
}

// TasksByRuling lists repository's cards citing design ruling ruling on
// their Ships line, oldest first, whatever their status, as the SQL
// store does: the review gate unions their stated clauses.
func (s *acceptanceStore) TasksByRuling(_ context.Context, repository, ruling string) ([]factoryd.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cards []factoryd.Task
	for _, t := range s.tasks {
		if t.Repository == repository && t.ShipsRuling == ruling {
			cards = append(cards, t)
		}
	}
	slices.SortFunc(cards, func(a, b factoryd.Task) int {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Compare(b.CreatedAt)
		}
		return strings.Compare(a.ID, b.ID)
	})
	return cards, nil
}

func (s *acceptanceStore) TaskByIssue(_ context.Context, repository string, issueNumber int64) (factoryd.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tasks {
		if t.Repository == repository && t.IssueNumber == issueNumber && issueNumber != 0 {
			return t, true, nil
		}
	}
	return factoryd.Task{}, false, nil
}

func (s *acceptanceStore) InsertIntakeEvent(_ context.Context, e factoryd.IntakeEvent) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.ContainsFunc(s.events, func(have factoryd.IntakeEvent) bool {
		return (e.DeliveryID != "" && have.DeliveryID == e.DeliveryID) ||
			(e.CommentID != 0 && have.TaskID == e.TaskID && have.Kind == e.Kind && have.CommentID == e.CommentID) ||
			(e.Kind == factoryd.IntakeHandover && have.TaskID == e.TaskID && have.Kind == factoryd.IntakeHandover)
	}) {
		return 0, nil
	}
	e.ID = int64(len(s.events) + 1)
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	s.events = append(s.events, e)
	return e.ID, nil
}

func (s *acceptanceStore) ConfirmFactoryClose(_ context.Context, taskID string, issue int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.ContainsFunc(s.events, func(e factoryd.IntakeEvent) bool {
		return e.TaskID == taskID && e.Kind == factoryd.IntakeFactoryClosed && e.CommentID == issue
	}) {
		return nil
	}
	if s.confirmedCloses == nil {
		s.confirmedCloses = map[[2]any]bool{}
	}
	s.confirmedCloses[[2]any{taskID, issue}] = true
	return nil
}

func (s *acceptanceStore) UnconfirmedFactoryCloses(_ context.Context, olderThan time.Duration, limit int) ([]factoryd.IntakeEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []factoryd.IntakeEvent
	for _, e := range s.events {
		if e.Kind == factoryd.IntakeFactoryClosed && !s.confirmedCloses[[2]any{e.TaskID, e.CommentID}] && time.Since(e.CreatedAt) > olderThan && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *acceptanceStore) ReleaseHandover(_ context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = slices.DeleteFunc(s.events, func(e factoryd.IntakeEvent) bool {
		return e.TaskID == taskID && e.Kind == factoryd.IntakeHandover
	})
	return nil
}

func (s *acceptanceStore) TouchedByPerson(_ context.Context, taskID string, bots ...string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.TaskID == taskID && e.Author != factoryd.FactoryDecider && !slices.Contains(bots, e.Author) {
			return true, nil
		}
	}
	return false, nil
}

func (s *acceptanceStore) IntakeEvents(_ context.Context, taskID string) ([]factoryd.IntakeEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []factoryd.IntakeEvent
	for _, e := range s.events {
		if e.TaskID == taskID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *acceptanceStore) SetSignalled(_ context.Context, id int64, signalled bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.events {
		if e.ID == id {
			s.events[i].Signalled = signalled
			return true, nil
		}
	}
	return false, nil
}

func (s *acceptanceStore) UnaddressedChangeRequests(_ context.Context, taskID string) ([]factoryd.IntakeEvent, error) {
	return nil, nil
}

func (s *acceptanceStore) SetTaskPR(_ context.Context, id string, number int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[id]
	t.PRNumber = number
	s.tasks[id] = t
	return nil
}

func (s *acceptanceStore) SetAgentSummary(_ context.Context, id, summary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[id]
	t.AgentSummary = summary
	s.tasks[id] = t
	return nil
}

func (s *acceptanceStore) CapRefusal(context.Context, string, time.Time) (*factoryd.CapRefusal, error) {
	return nil, nil
}

func (s *acceptanceStore) ProviderRefusal(context.Context, string, time.Time) (string, bool, error) {
	return "", false, nil
}

func (s *acceptanceStore) ModelAnswered(context.Context, string, time.Time) (bool, error) {
	return false, nil
}

func (s *acceptanceStore) CheckinSpent(context.Context, string) (float64, error) {
	return 0, nil
}

func (s *acceptanceStore) CheckinCalls(context.Context, string) (int, error) {
	return 0, nil
}

func (s *acceptanceStore) Free(_ context.Context, r policy.HoldRequest) (policy.TaskFree, policy.Free, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[r.TaskID]
	headroom, inf := t.BudgetEUR-t.SpentEUR, math.Inf(1)
	return policy.TaskFree{Status: t.Status, Headroom: headroom},
		policy.Free{Task: headroom, Day: inf, Month: inf, Project: inf}, nil
}

func (s *acceptanceStore) Opening(context.Context, string, string) (policy.Opening, error) {
	return policy.Opening{}, nil
}

func (s *acceptanceStore) SetRollHold(context.Context, string, time.Time, time.Time) error {
	return nil
}

func (s *acceptanceStore) ClearRollHold(context.Context, string) error { return nil }

func (s *acceptanceStore) SetTaskModelAlias(context.Context, string, string) error { return nil }

func (s *acceptanceStore) PauseQueue(context.Context, factoryd.QueuePause) error { return nil }

func (s *acceptanceStore) QueuePause(context.Context, string) (factoryd.QueuePause, bool, error) {
	return factoryd.QueuePause{}, false, nil
}

func (s *acceptanceStore) SetQueuePauseRevert(context.Context, string, string) error { return nil }

func (s *acceptanceStore) ClearQueuePause(context.Context, string) error { return nil }

func (s *acceptanceStore) PutLiveSession(context.Context, factoryd.LiveSession) error { return nil }

func (s *acceptanceStore) SetLiveSessionLines(context.Context, string, int) error { return nil }

func (s *acceptanceStore) LiveSession(context.Context, string) (factoryd.LiveSession, bool, error) {
	return factoryd.LiveSession{}, false, nil
}

func (s *acceptanceStore) DeleteLiveSession(context.Context, string) error { return nil }

func (s *acceptanceStore) RollHold(context.Context, time.Time) (factoryd.RollHold, bool, error) {
	return factoryd.RollHold{}, false, nil
}

func (s *acceptanceStore) EnterLane(_ context.Context, id, lane, class, sha string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok || t.Status != "backlog" {
		return false, nil
	}
	t.Lane, t.RequiredCapabilities, t.SHA, t.Status = lane, []string{class}, sha, "queued"
	s.tasks[id] = t
	return true, nil
}

func (s *acceptanceStore) ToHumanLane(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok || t.Kind != "agent" || t.Status != "queued" {
		return false, nil
	}
	t.Kind, t.Status = "human", "waiting-human"
	s.tasks[id] = t
	return true, nil
}

func (s *acceptanceStore) ProjectByRepository(context.Context, string) (factoryd.Project, bool, error) {
	return factoryd.Project{}, false, nil
}

func (s *acceptanceStore) SetTaskUsage(_ context.Context, id string, u factoryd.TaskUsage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[id]
	t.Usage = u
	s.tasks[id] = t
	return nil
}

func (s *acceptanceStore) DeliverySeen(context.Context, string) (bool, error) { return false, nil }

func (s *acceptanceStore) RecordDelivery(context.Context, string) error { return nil }

func (s *acceptanceStore) PruneDeliveries(context.Context, time.Duration) error { return nil }

func (s *acceptanceStore) InsertSession(_ context.Context, ssn factoryd.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = append(s.sessions, ssn)
	return nil
}

func (s *acceptanceStore) Session(context.Context, string) (factoryd.Session, bool, error) {
	return factoryd.Session{}, false, nil
}

func (s *acceptanceStore) UpdateSession(context.Context, string, []string, []byte) error { return nil }

func (s *acceptanceStore) SetTaskRisk(_ context.Context, id, class string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[id]
	t.RiskClass = class
	s.tasks[id] = t
	return nil
}

func (s *acceptanceStore) SetTaskAlertFingerprint(_ context.Context, id, fingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[id]
	t.AlertFingerprint = fingerprint
	s.tasks[id] = t
	return nil
}

func (s *acceptanceStore) SetTaskMergeCommit(_ context.Context, id, commit string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[id]
	t.MergeCommit = commit
	s.tasks[id] = t
	return nil
}

func (s *acceptanceStore) OpenApproval(_ context.Context, a factoryd.ApprovalRequest) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.ID = int64(len(s.approvals) + 1)
	s.approvals = append(s.approvals, a)
	return a.ID, nil
}

func (s *acceptanceStore) ResolveApprovals(_ context.Context, taskID, kind, status, by, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.approvals {
		if a.TaskID == taskID && (kind == "" || a.Kind == kind) && a.Status == "pending" {
			s.approvals[i].Status, s.approvals[i].DecidedBy, s.approvals[i].DecisionNote = status, by, note
		}
	}
	return nil
}

func (s *acceptanceStore) DecideApproval(context.Context, int64, string, string, string, string) error {
	return nil
}

func (s *acceptanceStore) UndecideApproval(context.Context, int64) error { return nil }

func (s *acceptanceStore) ReopenApproval(context.Context, string, string, string) error { return nil }

func (s *acceptanceStore) ApprovalRequests(context.Context, string) ([]factoryd.ApprovalRequest, error) {
	return nil, nil
}

func (s *acceptanceStore) SetGate(_ context.Context, id, gate string, since time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok || factoryd.Final(t.Status) {
		return nil
	}
	t.Gate, t.GateSince = gate, &since
	s.tasks[id] = t
	return nil
}

func (s *acceptanceStore) InsertAlert(_ context.Context, a factoryd.Alert) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.ID = int64(len(s.alerts) + 1)
	s.alerts = append(s.alerts, a)
	return a.ID, nil
}

func (s *acceptanceStore) AlertByFingerprint(_ context.Context, kind, fingerprint string) (factoryd.Alert, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.alerts) - 1; i >= 0; i-- {
		if a := s.alerts[i]; a.Kind == kind && a.Fingerprint == fingerprint {
			return a, true, nil
		}
	}
	return factoryd.Alert{}, false, nil
}

func (s *acceptanceStore) NewestAlertRows(_ context.Context, kind, namespace string) ([]factoryd.Alert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	newest := map[string]int{}
	for i, a := range s.alerts {
		if a.Kind == kind && a.Fingerprint != "" {
			newest[a.Fingerprint] = i
		}
	}
	var out []factoryd.Alert
	for i, a := range s.alerts {
		if a.Kind == kind && a.Fingerprint != "" && newest[a.Fingerprint] == i && (namespace == "" || a.Detail["namespace"] == namespace) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *acceptanceStore) MergeAlertDetail(_ context.Context, id int64, detail map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.alerts {
		if a.ID == id {
			if a.Detail == nil {
				a.Detail = map[string]any{}
			}
			for k, v := range detail {
				a.Detail[k] = v
			}
			s.alerts[i] = a
			return nil
		}
	}
	return nil
}

func (s *acceptanceStore) ResolveAlert(_ context.Context, id int64, kind, verdict, by string, detail map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.alerts {
		if a.ID == id && a.Kind == kind && a.Verdict == "" {
			now := time.Now()
			a.Verdict, a.ResolvedBy, a.ResolvedAt = verdict, by, &now
			if a.Detail == nil {
				a.Detail = map[string]any{}
			}
			for k, v := range detail {
				a.Detail[k] = v
			}
			s.alerts[i] = a
		}
	}
	return nil
}

func (s *acceptanceStore) JudgeEscalations(context.Context, string, factoryd.EscalationFilter, string, string) error {
	return nil
}

func (s *acceptanceStore) DayFree(_ context.Context, dayEUR float64) (float64, error) {
	return dayEUR, nil
}

func (s *acceptanceStore) JudgeTaskAlerts(_ context.Context, taskID, kind, verdict, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.alerts {
		if a.TaskID == taskID && a.Kind == kind && a.Verdict == "" {
			now := time.Now()
			s.alerts[i].Verdict, s.alerts[i].ResolvedBy, s.alerts[i].ResolvedAt = verdict, by, &now
		}
	}
	return nil
}

func (s *acceptanceStore) OpenReportTask(_ context.Context, t factoryd.Task) (factoryd.Task, error) {
	have, ok, _ := s.ReportOf(context.Background(), t.Parent, t.Source)
	if ok {
		return have, nil
	}
	return t, s.CreateTask(context.Background(), t)
}

func (s *acceptanceStore) ReportOf(_ context.Context, parent, source string) (factoryd.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tasks {
		if t.Parent == parent && t.Source == source {
			return t, true, nil
		}
	}
	return factoryd.Task{}, false, nil
}

func (s *acceptanceStore) PlanCards(context.Context, string) ([]factoryd.Task, error) {
	return nil, nil
}

func (s *acceptanceStore) SetTaskPlanRevision(context.Context, string, int) error {
	return nil
}

func (s *acceptanceStore) UpdatePlanCardBrief(context.Context, string, string) error {
	return nil
}

func (s *acceptanceStore) PutCriteriaVerdicts(context.Context, string, string, string, []factoryd.CriterionVerdict) error {
	return nil
}

func (s *acceptanceStore) CriteriaVerdicts(context.Context, string) ([]factoryd.CriterionVerdict, error) {
	return nil, nil
}

func (s *acceptanceStore) CriteriaVerdictTasks(context.Context) ([]string, error) { return nil, nil }

func (s *acceptanceStore) PlanCatches(context.Context, string) ([]factoryd.PlanCatch, error) {
	return nil, nil
}

func (s *acceptanceStore) AddTaskDeps(_ context.Context, id string, dependsOn []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deps[id] = append(s.deps[id], dependsOn...)
	return nil
}

func (s *acceptanceStore) OpenDependencies(_ context.Context, id string) ([]factoryd.Dependency, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []factoryd.Dependency
	for _, d := range s.deps[id] {
		if t := s.tasks[d]; t.Status != "done" {
			out = append(out, factoryd.Dependency{TaskID: d, Issue: t.IssueNumber, Key: t.PlanKey, Status: t.Status})
		}
	}
	return out, nil
}

func (s *acceptanceStore) WaitingDependents(context.Context, string) ([]string, error) {
	return nil, nil
}

func (s *acceptanceStore) ReplaceFailedCard(context.Context, string, factoryd.Task) (bool, error) {
	return false, nil
}

func (s *acceptanceStore) AddPlanRevision(context.Context, string, json.RawMessage, string) (int, error) {
	return 0, nil
}

func (s *acceptanceStore) PlanRevision(context.Context, string, int) (factoryd.PlanRevision, bool, error) {
	return factoryd.PlanRevision{}, false, nil
}

func (s *acceptanceStore) PutPlanQuestions(context.Context, string, int, []factoryd.PlanQuestion) (bool, error) {
	return false, nil
}

func (s *acceptanceStore) PutPlanConfirmation(context.Context, string, int, factoryd.PlanQuestion) (int, bool, error) {
	return 0, false, nil
}

func (s *acceptanceStore) PlanConfirmationAnswered(context.Context, string) (bool, error) {
	return false, nil
}

func (s *acceptanceStore) PutPlanCatches(context.Context, string, int, string, string, []string) (bool, error) {
	return false, nil
}

func (s *acceptanceStore) PlanQuestions(context.Context, string, int) ([]factoryd.PlanQuestion, error) {
	return nil, nil
}

func (s *acceptanceStore) AnswerPlanQuestion(context.Context, string, int, int, string, string) error {
	return nil
}

func (s *acceptanceStore) AnswerPlanQuestions(context.Context, string, int, []factoryd.PlanAnswer, string) error {
	return nil
}

func (s *acceptanceStore) OpenPlanQuestions(context.Context, string, int) (int, error) { return 0, nil }

func (s *acceptanceStore) ConfirmationIndex(context.Context, string, int) (int, bool, error) {
	return 0, false, nil
}

func (s *acceptanceStore) PutPlanHoldouts(context.Context, string, int, []factoryd.Holdout) (bool, error) {
	return false, nil
}

func (s *acceptanceStore) PlanHoldouts(context.Context, string) ([]factoryd.Holdout, error) {
	return nil, nil
}

func (s *acceptanceStore) UndecidedHoldouts(context.Context, string, int) (int, error) {
	return 0, nil
}

func (s *acceptanceStore) DecidePlanHoldout(context.Context, string, int, int, string) error {
	return nil
}

func (s *acceptanceStore) SweepPlanHoldouts(context.Context, string) error { return nil }

func (s *acceptanceStore) SetHoldoutAudit(context.Context, string, int, string, string, string) error {
	return nil
}

func (s *acceptanceStore) ClearHoldoutAudit(context.Context, string, int) error { return nil }

func (s *acceptanceStore) SetHoldoutTest(context.Context, string, int, string, string, string) error {
	return nil
}

func (s *acceptanceStore) BumpHoldoutRed(context.Context, string, int) (int, error) {
	return 0, nil
}

func (s *acceptanceStore) OpenHoldout(context.Context, string, int) (string, error) {
	return "", nil
}

func (s *acceptanceStore) PastReverts(context.Context, string) ([]string, error) {
	return nil, nil
}

func (s *acceptanceStore) CreatePlanCard(_ context.Context, t factoryd.Task) (factoryd.Task, error) {
	return t, s.CreateTask(context.Background(), t)
}

func (s *acceptanceStore) SetTaskIssue(_ context.Context, id string, issue int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[id]
	t.IssueNumber = issue
	s.tasks[id] = t
	return nil
}

func (s *acceptanceStore) UncommittedRolls(context.Context, string) ([]factoryd.RollRequest, error) {
	return nil, nil
}

func (s *acceptanceStore) TaskRolls(context.Context, string) ([]factoryd.RollRequest, error) {
	return nil, nil
}

func (s *acceptanceStore) MarkRollsCommitted(context.Context, string, []string, string) error {
	return nil
}

func (s *acceptanceStore) RollHead(context.Context, string) (string, error) { return "", nil }

func (s *acceptanceStore) RebaseRolls(context.Context, string, string) (bool, error) {
	return false, nil
}

func (s *acceptanceStore) SetTaskSHA(_ context.Context, id, sha string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[id]
	t.SHA = sha
	s.tasks[id] = t
	return nil
}

func (s *acceptanceStore) EnterQueue(_ context.Context, project, taskID string) (factoryd.QueueSpot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.Index(s.queue, taskID)
	spot := factoryd.QueueSpot{New: i < 0, EnteredAt: time.Now()}
	if i < 0 {
		s.queue = append(s.queue, taskID)
		i = len(s.queue) - 1
	}
	spot.Ahead = i
	return spot, nil
}

func (s *acceptanceStore) QueueHead(context.Context, string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return "", false, nil
	}
	return s.queue[0], true, nil
}

func (s *acceptanceStore) LeaveQueue(_ context.Context, taskID string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.Index(s.queue, taskID)
	if i < 0 {
		return "", false, nil
	}
	s.queue = slices.Delete(s.queue, i, i+1)
	return "demo-app/demo-app", true, nil
}

func (s *acceptanceStore) WaitingQueueHeads(context.Context) ([]string, error) { return nil, nil }

func (s *acceptanceStore) MonthSpend(context.Context, time.Time) (factoryd.MonthSpend, error) {
	return factoryd.MonthSpend{}, nil
}

func (s *acceptanceStore) Record(_ context.Context, e factoryd.Event) (int64, error) {
	return s.InsertIntakeEvent(context.Background(), factoryd.IntakeEvent{TaskID: e.TaskID, Kind: e.Kind, Author: e.Actor, CreatedAt: time.Now()})
}

// read-back helpers the acceptance assertions use

func (s *acceptanceStore) taskByID(id string) (factoryd.Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	return t, ok
}

func (s *acceptanceStore) statusOf(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tasks[id].Status
}

func (s *acceptanceStore) alertRowByFingerprint(fp string) factoryd.Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.alerts {
		if a.Fingerprint == fp {
			return a
		}
	}
	return factoryd.Alert{}
}

func (s *acceptanceStore) finalOf(taskID string) (final, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok || !factoryd.Final(t.Status) {
		return "", ""
	}
	return t.Status, ""
}

func (s *acceptanceStore) CountBacklog(context.Context, string) (int, error) { return 0, nil }

var _ factoryd.Store = (*acceptanceStore)(nil)

// RoutineRunOfTask: no routine files a card here.
func (s *acceptanceStore) RoutineRunOfTask(context.Context, string) (factoryd.RoutineRun, bool, error) {
	return factoryd.RoutineRun{}, false, nil
}

func (s *acceptanceStore) EndRoutineRun(context.Context, string, string, int, string, string, string, int64, float64) error {
	return nil
}

func (s *acceptanceStore) OverHoldCall(context.Context, string, time.Time) (*factoryd.OverHoldCall, error) {
	return nil, nil
}

func (s *acceptanceStore) BudgetRefused(context.Context, string, time.Time) (bool, error) {
	return false, nil
}
