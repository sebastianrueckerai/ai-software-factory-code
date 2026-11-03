package acceptance

// The gateway side of the pool's acceptance proofs (the pool-gateway
// rig): a real gateway.NewServer stood on in-memory fakes — the Store
// contract the gateway defines (Task, InsertAudit, CurrentPolicy,
// TakeHold, ReleaseHold) as a ledger whose TakeHold fits calls with
// policy.Fit exactly as the production store does, an Authenticator that
// names the calling task, a KeyMinter that records, a Pricer of the one
// alias the proofs call — and a stub LiteLLM that counts the backend
// calls none of these proofs may reach. The policy version in force is
// built from the code project's file text, so the proof's ceilings are
// the file's, its pool with them.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	factorygateway "github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"gopkg.in/yaml.v3"
)

// gwPoolHead is the code project's file a policy PR wrote for these
// proofs: the pool at EUR 10 under a project cap of EUR 100 — lower
// than the global month ceiling, so a proof can bind each ceiling on
// its own.
const gwPoolHead = "# the code project\nrepository: sf/code\nmerge_rule: human\nbudget:\n" +
	"  # the bounds the pool needs, which a policy PR writes\n  month_eur: 100\n  task_max_eur: 5\n" +
	"  raise_max_eur: 1\n  discretionary_eur: 10\n"

// gwPoolLanded is gwPoolHead after a confirmed top-up of EUR 1: only the
// pool moved; the comment, the caps and every other key stand.
const gwPoolLanded = "# the code project\nrepository: sf/code\nmerge_rule: human\nbudget:\n" +
	"  # the bounds the pool needs, which a policy PR writes\n  month_eur: 100\n  task_max_eur: 5\n" +
	"  raise_max_eur: 1\n  discretionary_eur: 11\n"

// The policy.yaml ceilings the proofs hold every call to: the day's and
// the month's, which no project file moves and neither can its pool.
const (
	gwDayEUR   = 100.0
	gwMonthEUR = 600.0
)

// gwProjectCapEUR is the code project's own month cap, its file's
// budget.month_eur: lower than the global month ceiling, so the project
// bound binds on its own.
const gwProjectCapEUR = 100.0

// accGatewayPolicy answers the policy version the gateway enforces, its
// code project read from the project file's own text — the file a pool
// change lands on — in the deployed shape of the gateway package's own
// test policy: the classes, limits, risk and envelope the gateway
// validates on load, the global ceilings fixed, the alias's output limit
// 32768. Only the file's budget keys move between versions of a proof.
func accGatewayPolicy(id int64, file string) (policy.Version, error) {
	var f struct {
		Repository string `yaml:"repository"`
		Budget     struct {
			MonthEUR         float64 `yaml:"month_eur"`
			TaskMaxEUR       float64 `yaml:"task_max_eur"`
			RaiseMaxEUR      float64 `yaml:"raise_max_eur"`
			DiscretionaryEUR float64 `yaml:"discretionary_eur"`
		} `yaml:"budget"`
	}
	if err := yaml.Unmarshal([]byte(file), &f); err != nil {
		return policy.Version{}, err
	}
	if f.Repository != "sf/code" || f.Budget.MonthEUR <= 0 || f.Budget.DiscretionaryEUR < 0 || f.Budget.RaiseMaxEUR <= 0 {
		return policy.Version{}, fmt.Errorf("the project file holds no budget the gateway loads: %q", file)
	}
	return policy.Version{ID: id, Commit: fmt.Sprintf("gw-%d", id), Tree: policy.Tree{
		Document: policy.Document{
			Version: 1,
			Budget:  policy.Budget{TaskDefaultEUR: 2, TaskMaxEUR: 5, DayEUR: gwDayEUR, MonthEUR: gwMonthEUR},
			Limits: policy.Limits{Sessions: 3, AgentTime: 20 * time.Minute, SupervisorCalls: 3,
				SupervisorTimeout: 6 * time.Minute, StrongerAfter: 1},
			Risk:            policy.Risk{Classes: []policy.RiskClass{{Name: "normal", Outcome: policy.OutcomeRequiresApproval}}, Default: "normal"},
			EnvelopeDefault: policy.Envelope{MonthlyEUR: 10, ServerTypes: []string{"cx23"}, MaxCount: 2, MaxLeaseHours: 4},
			Classes:         map[string]policy.Class{"coder": {Models: []string{"small", "big"}}},
			Prices:          policy.ToolPrices{MaxOutputTokens: map[string]int{"small": 32768, "big": 32768}},
		},
		Projects: map[string]policy.ProjectConfig{accProjectName: {Repository: f.Repository, MergeRule: "human",
			Budget: &policy.ProjectBudget{MonthEUR: f.Budget.MonthEUR, TaskMaxEUR: f.Budget.TaskMaxEUR,
				RaiseMaxEUR: f.Budget.RaiseMaxEUR, DiscretionaryEUR: f.Budget.DiscretionaryEUR}}},
	}}, nil
}

// gwCaller is the task whose call a proof posts: allocated EUR 100 of
// its own and none spent — a card's allocation is not spend — a running
// coder of the code project on the alias small.
var gwCaller = factorygateway.Task{ID: "t-gw", BudgetEUR: 100, Status: "running",
	Classes: []string{"coder"}, Repository: "sf/code", Source: "card", ModelAlias: "small"}

// gwOtherRepo and gwOwnRepo are the caller's sibling spenders: of
// another project, outside the code project's cap, and of the code
// project itself, inside it.
var (
	gwOtherRepo = factorygateway.Task{ID: "t-sib", BudgetEUR: 100, Status: "running",
		Classes: []string{"coder"}, Repository: "sf/other", Source: "card", ModelAlias: "small"}
	gwOwnRepo = factorygateway.Task{ID: "t-own", BudgetEUR: 100, Status: "running",
		Classes: []string{"coder"}, Repository: "sf/code", Source: "card", ModelAlias: "small"}
)

// gwLedgerRow is one booked ledger row, against the bounds it says: a
// day row is today's and so this month's; a month row was booked earlier
// in the same UTC month — the split the day's and the month's spend
// queries draw by date.
type gwLedgerRow struct {
	taskID, ref, kind string
	eur               float64
	day, month        bool
}

// accGatewayStore is the in-memory gateway.Store of these proofs:
// tasks, the ledger, the live holds and the audit rows under one mutex,
// with policy.Fit doing the admission exactly as the production store's
// TakeHold does: every outstanding hold counts against the day, the
// month and its task once, a row against the bounds it says, a
// project's rows against its cap. Model calls only — these proofs drive
// the chat completions route.
type accGatewayStore struct {
	mu     sync.Mutex
	tasks  map[string]factorygateway.Task
	rows   []gwLedgerRow
	usages []policy.Usage // each ledger row's
	holds  map[int64]policy.Hold
	nextID int64
	audits []factorygateway.AuditRow
	reqs   []policy.HoldRequest
	pol    *policy.Version // nil = none loaded
}

func newAccGatewayStore(tasks ...factorygateway.Task) *accGatewayStore {
	s := &accGatewayStore{tasks: map[string]factorygateway.Task{}, holds: map[int64]policy.Hold{}}
	for _, t := range tasks {
		s.tasks[t.ID] = t
	}
	return s
}

// seedDay books eur on taskID's ledger today, against the day and so
// the month; seedMonth books it earlier this UTC month, against the
// month alone; seedHold stands a live hold of eur on taskID, a call in
// flight.
func (s *accGatewayStore) seedDay(taskID string, eur float64) {
	s.seedRow(gwLedgerRow{taskID: taskID, ref: "small", kind: policy.KindModel, eur: eur, day: true, month: true}, policy.Usage{})
}

func (s *accGatewayStore) seedMonth(taskID string, eur float64) {
	s.seedRow(gwLedgerRow{taskID: taskID, ref: "small", kind: policy.KindModel, eur: eur, month: true}, policy.Usage{})
}

func (s *accGatewayStore) seedRow(l gwLedgerRow, u policy.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, l)
	s.usages = append(s.usages, u)
}

func (s *accGatewayStore) seedHold(taskID string, eur float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	s.holds[s.nextID] = policy.Hold{ID: s.nextID, TaskID: taskID, Kind: policy.KindModel, Ref: "small", EUR: eur}
}

func (s *accGatewayStore) setPolicy(v policy.Version) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pol = &v
}

func (s *accGatewayStore) Task(_ context.Context, id string) (factorygateway.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	return t, ok, nil
}

func (s *accGatewayStore) CurrentPolicy(context.Context) (policy.Version, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pol == nil {
		return policy.Version{}, false, nil
	}
	return *s.pol, true, nil
}

func (s *accGatewayStore) InsertAudit(_ context.Context, row factorygateway.AuditRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audits = append(s.audits, row)
	return nil
}

func (s *accGatewayStore) TakeHold(_ context.Context, r policy.HoldRequest) (policy.Hold, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[r.TaskID]
	if !ok {
		return policy.Hold{}, fmt.Errorf("hold: no task row for %s", r.TaskID)
	}
	if t.Status != "running" && t.Status != "in-review" {
		return policy.Hold{}, &policy.Refusal{Decision: "refused_task_status", Reason: "task " + t.ID + " is " + t.Status}
	}
	// the newest settled call on the ref calibrates the prompt, as the
	// production store reads it
	for i, l := range s.rows {
		if u := s.usages[i]; l.taskID == r.TaskID && l.kind == r.Kind && l.ref == r.Ref && u.PromptTokens > 0 && u.BodyBytes > 0 {
			r.Earlier = u
		}
	}
	s.reqs = append(s.reqs, r)
	day, month, project := 0.0, 0.0, 0.0
	for _, l := range s.rows {
		if l.day {
			day += l.eur
		}
		if l.month {
			month += l.eur
			if s.tasks[l.taskID].Repository == t.Repository {
				project += l.eur
			}
		}
	}
	taskHeld, dayHeld, projectHeld := 0.0, 0.0, 0.0
	for _, h := range s.holds {
		dayHeld += h.EUR
		if h.TaskID == r.TaskID {
			taskHeld += h.EUR
		}
		if s.tasks[h.TaskID].Repository == t.Repository {
			projectHeld += h.EUR
		}
	}
	free := policy.Free{Task: t.BudgetEUR - t.SpentEUR - taskHeld, Day: r.DayEUR - day - dayHeld,
		Month: r.MonthEUR - month - dayHeld, Project: r.ProjectEUR - project - projectHeld}
	h, err := policy.Fit(free, r)
	if err != nil {
		return h, err
	}
	s.nextID++
	h.ID = s.nextID
	s.holds[h.ID] = h
	return h, nil
}

func (s *accGatewayStore) ReleaseHold(_ context.Context, h policy.Hold, cost *float64, u policy.Usage) (policy.Booking, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cost = policy.KnownCost(cost)
	held, ok := s.holds[h.ID]
	delete(s.holds, h.ID)
	amount := held.EUR
	if cost != nil {
		amount = *cost
	}
	if !ok {
		amount = 0
		if cost != nil {
			amount = max(0, *cost-h.EUR)
		}
	}
	t := s.tasks[h.TaskID]
	if amount > 0 || ok && h.Kind == policy.KindTool {
		t.SpentEUR += amount
		s.tasks[h.TaskID] = t
		s.rows = append(s.rows, gwLedgerRow{taskID: h.TaskID, ref: h.Ref, kind: h.Kind, eur: amount, day: true, month: true})
		s.usages = append(s.usages, u)
	}
	b := policy.Booking{Spent: t.SpentEUR, Booked: amount}
	if cost != nil {
		b.OverHold = policy.OverHold(h.TaskID, h.Kind+" "+h.Ref, *cost, h.EUR)
	}
	return b, nil
}

// auditRows, holdRequests, openHolds and spent answer the proof's reads:
// the audit rows in order, the hold requests the handler fitted, the
// holds still live, a task's spent figure.
func (s *accGatewayStore) auditRows() []factorygateway.AuditRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]factorygateway.AuditRow(nil), s.audits...)
}

func (s *accGatewayStore) holdRequests() []policy.HoldRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]policy.HoldRequest(nil), s.reqs...)
}

func (s *accGatewayStore) openHolds() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.holds)
}

func (s *accGatewayStore) spent(taskID string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tasks[taskID].SpentEUR
}

var _ factorygateway.Store = (*accGatewayStore)(nil)

// accGatewayAuth names one task's sandbox on every request: the proof
// posts as that task's agent.
type accGatewayAuth struct{ task string }

func (a accGatewayAuth) Authenticate(*http.Request) (factorygateway.Identity, error) {
	return factorygateway.Identity{TaskID: a.task, SAUID: "sa-" + a.task}, nil
}

// accGatewayMinter records the keys the gateway mints: a refused call
// mints none, and a proof of refusals answers no mint at all.
type accGatewayMinter struct {
	mu    sync.Mutex
	mints []string
}

func (m *accGatewayMinter) Mint(_ context.Context, slot, taskID string, models []string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mints = append(m.mints, slot+" "+taskID+" "+strings.Join(models, ","))
	return fmt.Sprintf("gw-key-%d", len(m.mints)), nil
}

func (m *accGatewayMinter) Block(_ context.Context, _ string) error {
	return nil // no key of these proofs is ever blocked
}

func (m *accGatewayMinter) calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.mints...)
}

// accGatewayPrices prices the one alias the proofs call at the proof's
// own figure; every other alias the gateway refuses to price.
type accGatewayPrices struct{ price policy.Price }

func (p accGatewayPrices) Price(_ context.Context, alias string) (policy.Price, error) {
	if alias == "small" {
		return p.price, nil
	}
	return policy.Price{}, fmt.Errorf("LiteLLM has no model %q", alias)
}

// accStubLiteLLM counts the backend calls the gateway proxied — a proof
// of refusals answers none.
type accStubLiteLLM struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func newAccStubLiteLLM(t *testing.T) *accStubLiteLLM {
	t.Helper()
	s := &accStubLiteLLM{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}") // #nosec G104 -- nothing of these proofs reads the stub's answer
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *accStubLiteLLM) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}
