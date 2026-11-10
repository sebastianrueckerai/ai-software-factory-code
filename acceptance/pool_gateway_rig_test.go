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
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	factorygateway "github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"gopkg.in/yaml.v3"
)

// gwPoolBase is the code project's file a policy PR wrote for these
// proofs, its pool's money aside: a project cap of EUR 100, lower than
// the global month ceiling, so a proof can bind each ceiling on its own,
// and an escalation pool of 30 sessions.
const gwPoolBase = "# the code project\nrepository: sf/code\nmerge_rule: human\nbudget:\n  month_eur: 100\n  task_max_eur: 5\n" +
	"escalation:\n  # the pool every extend and fund draws from, which a policy PR writes\n  pool:\n    sessions: 30\n"

// gwPoolHead is gwPoolBase with the pool's money at EUR 10.
const gwPoolHead = gwPoolBase + "    eur: 10\n"

// gwPoolLanded is gwPoolHead after a confirmed top-up of EUR 1: only the
// pool's money moved, the comment, the caps and every other key stand.
const gwPoolLanded = gwPoolBase + "    eur: 11\n"

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
// 32768. Only the file's escalation pool moves between versions of a
// proof.
func accGatewayPolicy(id int64, file string) (policy.Version, error) {
	var f struct {
		Repository string `yaml:"repository"`
		Budget     struct {
			MonthEUR   float64 `yaml:"month_eur"`
			TaskMaxEUR float64 `yaml:"task_max_eur"`
		} `yaml:"budget"`
		Escalation struct {
			Pool struct {
				Sessions int     `yaml:"sessions"`
				EUR      float64 `yaml:"eur"`
			} `yaml:"pool"`
		} `yaml:"escalation"`
	}
	if err := yaml.Unmarshal([]byte(file), &f); err != nil {
		return policy.Version{}, err
	}
	if f.Repository != "sf/code" || f.Budget.MonthEUR <= 0 || f.Escalation.Pool.EUR <= 0 {
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
			Budget:     &policy.ProjectBudget{MonthEUR: f.Budget.MonthEUR, TaskMaxEUR: f.Budget.TaskMaxEUR},
			Escalation: &policy.Escalation{Pool: policy.Draw{Sessions: f.Escalation.Pool.Sessions, EUR: f.Escalation.Pool.EUR}}}},
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
// flight. A booked row moves the task's own spent figure with it, as the
// production store's booking transaction does, so a seeded row binds the
// task's budget too, not only the day's, the month's and the project's
// figures.
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
	if t, ok := s.tasks[l.taskID]; ok {
		t.SpentEUR += l.eur
		s.tasks[l.taskID] = t
	}
}

// assertTaskLedger is the rig's bookkeeping invariant, checked in every
// proof's setup: each task's spent figure is the sum of its ledger rows
// — seeding and booking move them together, as the production store's
// transaction does — so no seeded spend can leave a task's own budget
// loose while its day, month and project figures bind.
func (s *accGatewayStore) assertTaskLedger(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	sums := map[string]float64{}
	for _, l := range s.rows {
		sums[l.taskID] += l.eur
	}
	for id, task := range s.tasks {
		if math.Abs(task.SpentEUR-sums[id]) > 1e-9 {
			t.Errorf("task %s's spent figure %f, its ledger rows sum %f", id, task.SpentEUR, sums[id])
		}
	}
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

func (s *accGatewayStore) TaskCosts(_ context.Context, id string) (factorygateway.TaskCosts, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[id]; !ok {
		return factorygateway.TaskCosts{}, false, nil
	}
	var tc factorygateway.TaskCosts
	for _, r := range s.audits {
		if r.TaskID != id || r.Tool != "" {
			continue
		}
		tc.Calls++
		tc.PromptTokens += r.PromptTokens
		tc.CachedTokens += r.CachedTokens
		tc.OutputTokens += r.OutputTokens
	}
	return tc, true, nil
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

func (s *accGatewayStore) LatencySamples(context.Context, []string, time.Time) ([]policy.LatencySample, error) {
	return nil, nil
}

func (s *accGatewayStore) RecordAliasSwitch(context.Context, policy.AliasSwitch) error { return nil }

func (s *accGatewayStore) OpenAliasSwitches(context.Context) ([]policy.AliasSwitch, error) {
	return nil, nil
}

func (p accGatewayPrices) Deployments(_ context.Context, alias string) ([]string, error) {
	return []string{"proof/" + alias}, nil
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

// gwPost posts the proofs' chat request at the handler as the caller's
// sandbox and answers its recorder.
func gwPost(h http.Handler, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer the-sandbox-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestPoolGatewayRig is the rig's smoke proof, and this recovery card's
// whole proof: the real gateway handler stood on this file's fakes admits
// a call whose irreducible one-euro reservation binds nothing — the hold
// fitted at the actual handler boundary, the key minted, the backend
// called, the cost booked, the hold released — and the same call at the
// day cap, a sibling's synchronized ledger row and live hold having
// spent it to EUR 0.50 free, is refused by production policy.Fit with
// nothing minted, booked or proxied. The caller's seeded card-budget
// allocation (EUR 100) is not ledger spend: the fit's task figure stays
// the allocation and the day's, the month's and the project's stay
// their ceilings, so the call is admitted — were the allocation spend,
// every bound would read 0 and refuse. The complete fixed-cap matrix —
// each ceiling bound on its own, before and after a confirmed Top up —
// is the next card's, on this recovered rig.
func TestPoolGatewayRig(t *testing.T) {
	head, err := accGatewayPolicy(1, gwPoolHead)
	if err != nil {
		t.Fatal(err)
	}
	body, bodyBytes := gwChatBody(t), gwBodyBytes(t)

	// the nonbinding reservation: every bound loose, the call runs
	t.Run("admits a nonbinding reservation", func(t *testing.T) {
		store := newAccGatewayStore(gwCaller)
		store.setPolicy(head)
		store.assertTaskLedger(t)
		minter, stub := &accGatewayMinter{}, newAccStubLiteLLM(t)
		rec := gwPost(gwGateway(store, minter, accGatewayPrices{price: gwPrice(bodyBytes)}, stub.URL), body)
		if rec.Code != http.StatusOK {
			t.Fatalf("the admitted call = %d:\n%s", rec.Code, rec.Body.String())
		}
		if rec.Body.String() != "{}" {
			t.Fatalf("the admitted call's body = %q, want the stub's", rec.Body.String())
		}
		// the actual handler hold: one request, the version's ceilings
		reqs := store.holdRequests()
		if len(reqs) != 1 {
			t.Fatalf("the handler fitted %d hold requests, want the one", len(reqs))
		}
		gwHoldRequest(t, "the admitted call", reqs[0], bodyBytes)
		// the backend activity: one key minted, one call proxied
		equal(t, "the minted keys", minter.calls(), []string{gwCaller.ID + " " + gwCaller.ID + " big,big-alibaba,small,small-alibaba"})
		equal(t, "the backend calls", stub.calls(), []string{"POST /v1/chat/completions"})
		// the booking: the hold released into the one ledger row, at the
		// hold's full amount — the stub reports no cost — and the
		// allocation of EUR 100 books nothing itself
		if got := store.openHolds(); got != 0 {
			t.Fatalf("the admitted call left %d holds open, want none", got)
		}
		booked := store.spent(gwCaller.ID)
		rows := store.auditRows()
		if len(rows) != 1 {
			t.Fatalf("the admitted call wrote %d audit rows, want the one", len(rows))
		}
		row := rows[0]
		limit := 32768
		equal(t, "the admitted call's audit row",
			[]any{row.TaskID, row.Kind, row.Decision, row.ModelAlias, row.PolicyVersion, row.OutputLimit, row.Status, row.CallCostEUR},
			[]any{gwCaller.ID, "", "allowed", "small", &head.ID, &limit, "200", (*float64)(nil)})
		want := float64(reqs[0].Bytes)*reqs[0].Price.In + float64(limit)*reqs[0].Price.Out
		for what, got := range map[string]float64{"the booked spend": booked, "the audit row's hold": *row.HoldEUR, "the audit row's spend": *row.SpendEUR} {
			if math.Abs(got-want) > 1e-9 {
				t.Errorf("%s = %f, want the hold's %f", what, got, want)
			}
		}
		if row.Reason != "cost not reported, booked the hold "+strconv.FormatFloat(want, 'f', 6, 64)+" EUR" {
			t.Errorf("the admitted call's audit reason = %q", row.Reason)
		}
	})

	// the binding day cap: the sibling's row and live hold leave EUR 0.50
	// of the day, and the same one-euro reservation no longer fits
	t.Run("refuses the same reservation at the day cap", func(t *testing.T) {
		store := newAccGatewayStore(gwCaller, gwOtherRepo)
		store.seedDay(gwOtherRepo.ID, 98.5)
		store.seedHold(gwOtherRepo.ID, 1)
		store.setPolicy(head)
		store.assertTaskLedger(t)
		minter, stub := &accGatewayMinter{}, newAccStubLiteLLM(t)
		h := gwGateway(store, minter, accGatewayPrices{price: gwPrice(bodyBytes)}, stub.URL)
		req := gwRefused(t, "the day cap", h, body, store, minter, stub, head.ID, policy.RefusedDayCap,
			"free headroom 0.500000 EUR (task 100.000000, day 0.500000, month 500.500000, project 100.000000) "+
				"fits 0 output tokens after a request of "+strconv.Itoa(bodyBytes)+" bytes (at most "+strconv.Itoa(bodyBytes)+" prompt tokens), "+
				"below the minimum call of "+strconv.Itoa(policy.MinCallTokens))
		gwHoldRequest(t, "the day cap", req, bodyBytes)
	})
}
