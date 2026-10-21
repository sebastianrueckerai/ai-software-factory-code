// Package acceptance holds the acceptance tests of the costs read's date
// window (plan #507, card costs-date-integration): asked 'spend per card
// today' through the chat, the costs tool answers with hand-derived day
// sums from the fixture ledger; a custom window sums both days; no dates
// keep the current month; invalid dates are refused.
package acceptance

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/console"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	gwpack "github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The fixture ledger: 2026-10-10 spends t-m's small 0.5 and search
// 0.1 (day total 0.6); 2026-10-11 spends t-r's big 1.2 and
// review/big 0.3 and t-m's search 0.2 (day total 1.7). By
// card on 2026-10-11 alone: t-r 1.5 + t-m 0.8 = 2.3000 EUR; over both
// days the grand total is 0.6 + 1.7 = 2.3000 EUR by hand too. Each line
// names its day behind its ref, which the fake ledger filters on.
var fixtureDays = []policy.DayCost{
	{Day: "2026-10-10", ByKind: map[string]float64{policy.KindModel: 0.5, policy.KindTool: 0.1}, Total: 0.6},
	{Day: "2026-10-11", ByKind: map[string]float64{policy.KindModel: 1.5, policy.KindTool: 0.2}, Total: 1.7},
}

var fixtureTasks = []policy.TaskCost{
	{TaskID: "t-r", EUR: 1.5, Lines: []policy.CostLine{{Kind: policy.KindModel, Ref: "big 2026-10-11", EUR: 1.2},
		{Kind: policy.KindModel, Ref: "review/big 2026-10-11", EUR: 0.3}}},
	{TaskID: "t-m", EUR: 0.8, Lines: []policy.CostLine{{Kind: policy.KindModel, Ref: "small 2026-10-11", EUR: 0.6},
		{Kind: policy.KindTool, Ref: "search 2026-10-11", EUR: 0.2}}},
}

const daySumCard = "By card 2026-10-11 to 2026-10-11, EUR:\n  t-r 1.5000\n  t-m 0.8000\ntotal: 2.3000 EUR"

const windowSumDay = "By day 2026-10-10 to 2026-10-11, EUR:\n  2026-10-11 1.7000 (model 1.5000, tool 0.2000)\n  2026-10-10 0.6000 (model 0.5000, tool 0.1000)\ntotal: 2.3000 EUR"

const badWindowRefusal = "Refused: from and to must be dates YYYY-MM-DD, from not after to"

// acceptLedger is the fixture ledger: Costs answers the fixture's rows
// for the window it is asked and records each window in order.
type acceptLedger struct {
	mu   sync.Mutex
	wins [][2]string
	chat *policy.ChatConfig
}

// Costs answers the fixture's rows for the window it is asked, as the
// ledger store does: only the days whose day falls in the window, each
// card's lines with a day in the window summed into its EUR and kept as
// its lines, and the window's own From and To on the head. The fixture's
// lines carry their day in Ref behind a space: "small 2026-10-10".
// DaySpend is the fixture's total of one day, the board's figure; it is
// no window read.
func (l *acceptLedger) DaySpend(_ context.Context, day time.Time) (float64, error) {
	var eur float64
	for _, d := range fixtureDays {
		if d.Day == day.Format(policy.DateLayout) {
			eur += d.Total
		}
	}
	return eur, nil
}

func (l *acceptLedger) Costs(_ context.Context, from, to time.Time) (policy.Costs, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, t := from.Format(policy.DateLayout), to.Format(policy.DateLayout)
	l.wins = append(l.wins, [2]string{f, t})
	c := policy.Costs{From: f, To: t, Days: []policy.DayCost{}, Tasks: []policy.TaskCost{}}
	for _, d := range fixtureDays {
		if d.Day >= f && d.Day <= t {
			c.Days = append(c.Days, d)
		}
	}
	for _, task := range fixtureTasks {
		var eur float64
		var lines []policy.CostLine
		for _, line := range task.Lines {
			day := line.Ref[strings.LastIndexByte(line.Ref, ' ')+1:]
			if day >= f && day <= t {
				eur += line.EUR
				lines = append(lines, line)
			}
		}
		if eur == 0 {
			continue
		}
		c.Tasks = append(c.Tasks, policy.TaskCost{TaskID: task.TaskID, EUR: eur, Lines: lines})
	}
	return c, nil
}

func (l *acceptLedger) ModelCalls(_ context.Context, _ string) ([]policy.ModelCall, error) {
	return nil, nil
}

func (l *acceptLedger) CallTotals(context.Context, []string) (map[string]policy.CallTotals, error) {
	return nil, nil
}

func (l *acceptLedger) windows() [][2]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.wins)
}

func (l *acceptLedger) CurrentPolicy(context.Context) (policy.Version, bool, error) {
	l.mu.Lock()
	ch := l.chat
	l.mu.Unlock()
	v := policy.Version{ID: 4, Commit: "c9d98bb0aa11", LoadedAt: time.Date(2026, 9, 27, 22, 50, 7, 0, time.UTC),
		ActivatedAt: time.Date(2026, 9, 27, 22, 56, 38, 0, time.UTC),
		Tree: policy.Tree{Document: policy.Document{Version: 1, Budget: policy.Budget{TaskDefaultEUR: 2, TaskMaxEUR: 5, DayEUR: 20, MonthEUR: 50},
			Classes:         map[string]policy.Class{"coder": {Models: []string{"factory/small"}}},
			Limits:          policy.Limits{Sessions: 3, AgentTime: 20 * time.Minute, SupervisorCalls: 3, SupervisorTimeout: 6 * time.Minute, StrongerAfter: 1},
			Approvers:       []string{"sebastian"},
			EnvelopeDefault: policy.Envelope{MonthlyEUR: 10, ServerTypes: []string{"cx23"}, MaxCount: 2, MaxLeaseHours: 4, VolumeMaxGB: 20},
			Chat:            ch}}}
	return v, true, nil
}

// acceptSessions are the console_sessions rows in memory.
type acceptSessions struct {
	mu   sync.Mutex
	rows map[string]factoryd.ConsoleSession
	now  func() time.Time
}

func newAcceptSessions() *acceptSessions {
	return &acceptSessions{rows: map[string]factoryd.ConsoleSession{}, now: time.Now}
}

func (f *acceptSessions) SaveConsoleSession(_ context.Context, sess factoryd.ConsoleSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[sess.IDHash] = sess
	return nil
}

func (f *acceptSessions) ConsoleSessionByHash(_ context.Context, hash string) (factoryd.ConsoleSession, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[hash]
	if !ok {
		return factoryd.ConsoleSession{}, false, nil
	}
	if f.now().After(row.ExpiresAt) {
		delete(f.rows, hash)
		return factoryd.ConsoleSession{}, false, nil
	}
	return row, true, nil
}

func (f *acceptSessions) DeleteConsoleSession(_ context.Context, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, hash)
	return nil
}

func (f *acceptSessions) TouchConsoleSession(_ context.Context, hash string, seenAt time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[hash]
	if !ok || !row.LastSeen.Equal(seenAt) {
		return false, nil
	}
	row.LastSeen = f.now()
	f.rows[hash] = row
	return true, nil
}

func (f *acceptSessions) UpdateConsoleSessionTokens(_ context.Context, hash, access, refresh string, expiry time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[hash]
	if !ok {
		return errors.New("no session")
	}
	row.Access, row.Refresh, row.TokenExpiry = access, refresh, expiry
	f.rows[hash] = row
	return nil
}

func (f *acceptSessions) PurgeConsoleSessions(_ context.Context, at time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for h, r := range f.rows {
		if r.ExpiresAt.Before(at) {
			delete(f.rows, h)
			n++
		}
	}
	return n, nil
}

// acceptTasks are the board's task rows: the code project's two cards.
type acceptTasks struct {
	mu    sync.Mutex
	tasks []factoryd.BoardTask
}

func (s *acceptTasks) Task(_ context.Context, id string) (factoryd.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tasks {
		if t.ID == id {
			return t.Task, true, nil
		}
	}
	return factoryd.Task{}, false, nil
}

func (s *acceptTasks) TaskStatuses(_ context.Context, ids []string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, id := range ids {
		for _, t := range s.tasks {
			if t.ID == id {
				out[id] = t.Status
			}
		}
	}
	return out, nil
}

func (s *acceptTasks) TaskByIssue(_ context.Context, repository string, issue int64) (factoryd.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tasks {
		if t.Repository == repository && t.IssueNumber == issue {
			return t.Task, true, nil
		}
	}
	return factoryd.Task{}, false, nil
}

// BoardTasks lists repository's open cards, as the store does, in its
// order: a chat task is none.
func (s *acceptTasks) BoardTasks(_ context.Context, repository string) ([]factoryd.BoardTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.DeleteFunc(slices.Clone(s.tasks), func(t factoryd.BoardTask) bool {
		return t.Repository != repository || t.Kind == factoryd.KindChat || factoryd.Final(t.Status)
	})
	factoryd.SortBoardTasks(out)
	return out, nil
}

func (s *acceptTasks) FinishedTasks(_ context.Context, q factoryd.FinishedQuery) ([]factoryd.BoardTask, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	page, rest := factoryd.FinishedPage(s.tasks, q)
	return page, rest, nil
}

func (s *acceptTasks) BoardTask(_ context.Context, id string) (factoryd.BoardTask, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tasks {
		if t.ID == id {
			return t, true, nil
		}
	}
	return factoryd.BoardTask{}, false, nil
}

func (s *acceptTasks) PlanRevision(_ context.Context, _ string, _ int) (factoryd.PlanRevision, bool, error) {
	return factoryd.PlanRevision{}, false, nil
}

func (s *acceptTasks) PlanQuestions(_ context.Context, _ string, _ int) ([]factoryd.PlanQuestion, error) {
	return nil, nil
}

func (s *acceptTasks) PlanHoldouts(_ context.Context, _ string) ([]factoryd.Holdout, error) {
	return nil, nil
}

func (s *acceptTasks) PlanCards(_ context.Context, _ string) ([]factoryd.Task, error) {
	return nil, nil
}

func (s *acceptTasks) CriteriaVerdicts(_ context.Context, _ string) ([]factoryd.CriterionVerdict, error) {
	return nil, nil
}

func (s *acceptTasks) Dependencies(_ context.Context, _ string) ([]factoryd.Dependency, error) {
	return nil, nil
}

func (s *acceptTasks) Dependents(_ context.Context, _ string) ([]factoryd.Dependency, error) {
	return nil, nil
}
func (s *acceptTasks) RollCarries(_ context.Context, _ string) ([]factoryd.Task, error) {
	return nil, nil
}

func (s *acceptTasks) moveCard(taskID, lane string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.tasks {
		if t.ID == taskID {
			s.tasks[i].Lane = lane
			if lane == policy.LaneReady {
				s.tasks[i].Status = "queued"
			} else {
				s.tasks[i].Status = lane
			}
			break
		}
	}
}

func (s *acceptTasks) SetHumanSummary(_ context.Context, _, _ string) error { return nil }
func (s *acceptTasks) SetTaskAccent(_ context.Context, _, _ string) error   { return nil }
func (s *acceptTasks) SetObjective(_ context.Context, _, _ string) error    { return nil }
func (s *acceptTasks) RaiseTaskBudget(_ context.Context, _ string, _ float64) (float64, float64, error) {
	return 0, 0, nil
}
func (s *acceptTasks) FundCard(_ context.Context, _ string, _ float64, _ *factoryd.FundBounds, _ factoryd.Event) (float64, float64, error) {
	return 0, 0, nil
}
func (s *acceptTasks) AddTaskDeps(_ context.Context, _ string, _ []string) error {
	return nil
}
func (s *acceptTasks) RemoveTaskDeps(_ context.Context, _ string, _ []string) error {
	return nil
}

func (s *acceptTasks) add(t factoryd.Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks = append(s.tasks, factoryd.BoardTask{Task: t})
}

func (*acceptTasks) CountAdmitted(context.Context, string) (int, error) { return 0, nil }

// acceptObjects is the object store in memory.
type acceptObjects map[string][]byte

func (o acceptObjects) Put(_ context.Context, k string, b []byte) error { o[k] = b; return nil }
func (o acceptObjects) Get(_ context.Context, k string) ([]byte, error) { return o[k], nil }
func (o acceptObjects) Delete(_ context.Context, k string) error        { delete(o, k); return nil }

// acceptEvents is the event spine in memory.
type acceptEvents struct {
	mu    sync.Mutex
	rows  []factoryd.Event
	next  int64
	bus   *factoryd.EventBus
	tasks *acceptTasks
}

func (*acceptTasks) CountBacklog(context.Context, string) (int, error) { return 0, nil }

func (f *acceptEvents) Record(_ context.Context, e factoryd.Event) (int64, error) {
	if e.Repository == "" || e.Kind == "" {
		return 0, fmt.Errorf("event: repository and kind are required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	e.ID = f.next
	if e.At.IsZero() {
		e.At = time.Now()
	}
	f.rows = append(f.rows, e)
	f.bus.Publish(e)
	if e.Kind == factoryd.EventCardMoved && f.tasks != nil {
		if lane, ok := e.Detail["lane"].(string); ok {
			f.tasks.moveCard(e.TaskID, lane)
		}
	}
	return e.ID, nil
}

func (f *acceptEvents) ProjectEvents(_ context.Context, repository string, after int64, limit int) ([]factoryd.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []factoryd.Event
	for i := len(f.rows) - 1; i >= 0 && len(out) < limit; i-- {
		if r := f.rows[i]; r.Repository == repository && r.ID > after {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *acceptEvents) FiledPR(_ context.Context, name string) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.rows) - 1; i >= 0; i-- {
		if r := f.rows[i]; r.Repository == name+"/"+name && r.Kind == factoryd.EventProjectFiled {
			pr, _ := r.Detail["pr"].(int64)
			return pr, pr > 0, nil
		}
	}
	return 0, false, nil
}

func (f *acceptEvents) TaskEvents(_ context.Context, id string, limit int) ([]factoryd.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []factoryd.Event
	for i := len(f.rows) - 1; i >= 0 && len(out) < limit; i-- {
		if f.rows[i].TaskID == id {
			out = append(out, f.rows[i])
		}
	}
	return out, nil
}

func (f *acceptEvents) TaskEventsFor(ctx context.Context, ids []string, limit int) (map[string][]factoryd.Event, error) {
	out := map[string][]factoryd.Event{}
	for _, id := range ids {
		evs, err := f.TaskEvents(ctx, id, limit)
		if err != nil {
			return nil, err
		}
		out[id] = evs
	}
	return out, nil
}

func (f *acceptEvents) NewestTaskEvents(_ context.Context, tasks []string) (map[string]factoryd.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	set := map[string]bool{}
	for _, t := range tasks {
		set[t] = true
	}
	out := map[string]factoryd.Event{}
	for i := len(f.rows) - 1; i >= 0; i-- {
		e := f.rows[i]
		if e.TaskID != "" && set[e.TaskID] {
			if _, ok := out[e.TaskID]; !ok {
				out[e.TaskID] = e
			}
		}
	}
	return out, nil
}

// acceptResources, acceptAlerts, acceptApprovals, acceptProjects and
// acceptMirrors are the other ports' fixtures: empty rows, no calls.
type acceptResources struct{ rows []cloudres.Resource }

func (f *acceptResources) Commissions(_ context.Context, _ time.Time) ([]cloudres.Resource, error) {
	return f.rows, nil
}

func (f *acceptResources) Use(_ context.Context, _ string) (cloudres.EnvelopeUse, error) {
	return cloudres.EnvelopeUse{}, nil
}

func (f *acceptResources) Standing(_ context.Context, _ string) (cloudres.StandingUse, error) {
	return cloudres.StandingUse{}, nil
}

func (f *acceptResources) Promote(_ context.Context, _ int64, _ string, _ func(repo string) policy.Standing) error {
	return nil
}

func (f *acceptResources) EndLease(_ context.Context, _ int64, _ string) error { return nil }

func (f *acceptResources) Commission(_ context.Context, _ int64) (cloudres.Resource, bool, error) {
	return cloudres.Resource{}, false, nil
}

type acceptAlerts struct{ rows []factoryd.Alert }

func (a *acceptAlerts) Alerts(_ context.Context, limit int) ([]factoryd.Alert, error) {
	return a.rows[:min(limit, len(a.rows))], nil
}

func (a *acceptAlerts) SetAlertVerdict(_ context.Context, _ int64, _, _ string) error {
	return factoryd.ErrNotFound
}

func (a *acceptAlerts) AlertByID(_ context.Context, _ int64) (factoryd.Alert, bool, error) {
	return factoryd.Alert{}, false, nil
}

type acceptApprovals struct{ rows []factoryd.ApprovalRequest }

func (a *acceptApprovals) ApprovalRequests(_ context.Context, status string) ([]factoryd.ApprovalRequest, error) {
	var out []factoryd.ApprovalRequest
	for _, r := range a.rows {
		if r.Status == status {
			out = append(out, r)
		}
	}
	return out, nil
}

func (a *acceptApprovals) DecideApproval(_ context.Context, _ int64, _, _, _, _ string) error {
	return factoryd.ErrNotFound
}

type acceptProjects struct{ rows []factoryd.Project }

func (p *acceptProjects) Projects(context.Context) ([]factoryd.Project, error) { return p.rows, nil }

type acceptMirrors struct {
	mu   sync.Mutex
	rows map[string]factoryd.Mirror
	push map[string][]factoryd.PushMirror
}

func (m *acceptMirrors) ClearGitHubFailures(_ context.Context, _ string) error { return nil }

func (m *acceptMirrors) Mirror(_ context.Context, project, direction string) (factoryd.Mirror, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[project+" "+direction]
	r.Token = ""
	return r, ok, nil
}

func (m *acceptMirrors) MirrorToken(_ context.Context, project, direction string) (factoryd.Mirror, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[project+" "+direction]
	return r, ok, nil
}

func (m *acceptMirrors) PutMirror(_ context.Context, mm factoryd.Mirror) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[mm.Project+" "+mm.Direction] = mm
	return nil
}

func (m *acceptMirrors) SetImportPR(_ context.Context, _ string, _ int64) error { return nil }

func (m *acceptMirrors) DeleteMirror(_ context.Context, project, direction string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, project+" "+direction)
	return nil
}

func (m *acceptMirrors) PushMirrors(_ context.Context, repo factoryd.Repo) ([]factoryd.PushMirror, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.push[repo.String()]), nil
}

func (m *acceptMirrors) AddPushMirror(_ context.Context, repo factoryd.Repo, address, username, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := repo.String()
	m.push[key] = append(m.push[key], factoryd.PushMirror{RemoteName: fmt.Sprintf("remote_mirror_%d", len(m.push[key])), RemoteAddress: address})
	return nil
}

func (m *acceptMirrors) SyncPushMirrors(_ context.Context, _ factoryd.Repo) error { return nil }

func (m *acceptMirrors) DeletePushMirror(_ context.Context, repo factoryd.Repo, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := repo.String()
	m.push[key] = slices.DeleteFunc(m.push[key], func(p factoryd.PushMirror) bool { return p.RemoteName == name })
	return nil
}

// acceptChat is the chat store in memory: messages by repository and
// login, conversations numbered from 1, proposals by token.
type acceptChat struct {
	mu        sync.Mutex
	msgs      map[string][]factoryd.ChatMessage
	atts      map[string][]factoryd.ChatAttachment
	convs     map[string]int64
	next      int64
	at        time.Time
	proposals []acceptProposal
	tasks     *acceptTasks
}

type acceptProposal struct {
	factoryd.ChatProposal
	used time.Time
}

func newAcceptChat(tasks *acceptTasks) *acceptChat {
	return &acceptChat{msgs: map[string][]factoryd.ChatMessage{}, atts: map[string][]factoryd.ChatAttachment{}, convs: map[string]int64{},
		at: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC), tasks: tasks}
}

func (f *acceptChat) EnsureChatTask(ctx context.Context, name, repository string, budget float64, at time.Time) (factoryd.Task, error) {
	id := factoryd.ChatTaskID(name, at)
	if t, ok, _ := f.tasks.Task(ctx, id); ok {
		return t, nil
	}
	t := factoryd.Task{ID: id, Kind: factoryd.KindChat, Status: "running", BudgetEUR: budget, Repository: repository}
	f.tasks.add(t)
	return t, nil
}

func (f *acceptChat) current(repository, login string) int64 {
	if n, ok := f.convs[repository+" "+login]; ok && n > 0 {
		return n
	}
	return 1
}

func (f *acceptChat) AddChatMessage(_ context.Context, repository, login string, m factoryd.ChatMessage) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	m.ID, m.At, m.Conversation = f.next, f.at.Add(time.Duration(f.next)*time.Second), cmp.Or(m.Conversation, f.current(repository, login))
	k := repository + " " + login
	f.msgs[k] = append(f.msgs[k], m)
	if m.Conversation > f.convs[k] {
		f.convs[k] = m.Conversation
	}
	return m.ID, nil
}

func (f *acceptChat) AddChatAttachment(_ context.Context, repository, login string, a factoryd.ChatAttachment) (factoryd.ChatAttachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	a.ID, a.At = f.next, f.at.Add(time.Duration(f.next)*time.Second)
	f.atts[repository+" "+login] = append(f.atts[repository+" "+login], a)
	return a, nil
}

func (f *acceptChat) ChatAttachments(_ context.Context, repository, login string, conversation int64) ([]factoryd.ChatAttachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []factoryd.ChatAttachment
	for _, a := range f.atts[repository+" "+login] {
		if a.Conversation == conversation {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *acceptChat) ChatAttachmentByID(_ context.Context, repository string, id int64) (factoryd.ChatAttachment, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, atts := range f.atts {
		for _, a := range atts {
			if a.ID == id {
				return a, true, nil
			}
		}
	}
	return factoryd.ChatAttachment{}, false, nil
}

func (f *acceptChat) Conversation(_ context.Context, repository, login string, n int64) (factoryd.ChatConversation, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := factoryd.ChatConversation{Number: n, State: factoryd.ThreadActive, Current: n == f.current(repository, login)}
	for _, m := range f.msgs[repository+" "+login] {
		if m.Conversation == n {
			c.Messages++
		}
	}
	return c, c.Messages > 0, nil
}

func (f *acceptChat) OpenConversation(_ context.Context, repository, login string, n int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.convs[repository+" "+login] = n
	return nil
}

func (f *acceptChat) RenameConversation(context.Context, string, string, int64, string) (bool, error) {
	return true, nil
}

func (f *acceptChat) ModelTitle(context.Context, string, string, int64, string, string) (bool, error) {
	return true, nil
}

func (f *acceptChat) MarkSeen(context.Context, string, string, int64) error { return nil }

func (f *acceptChat) MarkCleared(context.Context, string, string, int64) error { return nil }

func (f *acceptChat) EnsureInbox(context.Context, string, string) (int64, error) { return 0, nil }

func (f *acceptChat) EnsureCardThread(context.Context, string, string, string) (int64, error) {
	return 0, nil
}

func (f *acceptChat) CardThreadFor(context.Context, string, string) (string, int64, bool, error) {
	return "", 0, false, nil
}

func (f *acceptChat) CardThreads(context.Context, string, []string) (map[string]factoryd.CardThread, error) {
	return map[string]factoryd.CardThread{}, nil
}

func (f *acceptChat) AddChatWatch(context.Context, string, string, int64, string) error { return nil }

func (f *acceptChat) RemoveChatWatch(context.Context, string, string, string) (bool, error) {
	return true, nil
}

func (f *acceptChat) AddChatTimer(context.Context, string, string, int64, time.Time, string, string) error {
	return nil
}

func (f *acceptChat) ArchiveConversation(context.Context, string, string, int64, bool) (bool, error) {
	return true, nil
}

func (f *acceptChat) SetChatIntake(context.Context, string, bool, string, time.Time) error {
	return nil
}

func (f *acceptChat) ChatIntakeClosed(context.Context, string) (bool, error) {
	return false, nil
}

func (f *acceptChat) AbandonChatMessage(context.Context, string, string, int64, int64) ([]int64, error) {
	return nil, nil
}
func (f *acceptChat) DeleteConversation(context.Context, string, string, int64) (int64, []int64, error) {
	return 0, nil, nil
}

func (f *acceptChat) StartQueued(_ context.Context, repository, login string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := repository + " " + login
	i := slices.IndexFunc(f.msgs[k], func(m factoryd.ChatMessage) bool { return m.ID == id && m.Queued })
	if i < 0 {
		return nil
	}
	m := f.msgs[k][i]
	f.msgs[k] = slices.Delete(f.msgs[k], i, i+1)
	f.next++
	m.ID, m.At, m.Queued = f.next, f.at.Add(time.Duration(f.next)*time.Second), false
	f.msgs[k] = append(f.msgs[k], m)
	return nil
}

func (f *acceptChat) CurrentConversation(_ context.Context, repository, login string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current(repository, login), nil
}

func (f *acceptChat) NewConversation(_ context.Context, repository, login string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	highest := int64(1)
	for _, m := range f.msgs[repository+" "+login] {
		highest = max(highest, m.Conversation)
	}
	f.convs[repository+" "+login] = highest + 1
	return highest + 1, nil
}

func (f *acceptChat) ChatAsks(context.Context, string, string) ([]factoryd.ChatMessage, error) {
	return nil, nil
}

func (f *acceptChat) Conversations(_ context.Context, repository, login string) ([]factoryd.ChatConversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []factoryd.ChatConversation
	for _, m := range f.msgs[repository+" "+login] {
		i := slices.IndexFunc(out, func(c factoryd.ChatConversation) bool { return c.Number == m.Conversation })
		if i < 0 {
			out, i = append(out, factoryd.ChatConversation{Number: m.Conversation, FirstAt: m.At}), len(out)
		}
		out[i].LastAt = m.At
		out[i].Messages++
	}
	slices.SortFunc(out, func(a, b factoryd.ChatConversation) int { return cmp.Compare(b.Number, a.Number) })
	return out, nil
}

func (f *acceptChat) ProjectConversations(_ context.Context, repository string) ([]factoryd.ChatConversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []factoryd.ChatConversation
	for key, msgs := range f.msgs {
		repo, login, _ := strings.Cut(key, " ")
		if repo != repository {
			continue
		}
		for _, m := range msgs {
			i := slices.IndexFunc(out, func(c factoryd.ChatConversation) bool { return c.Login == login && c.Number == m.Conversation })
			if i < 0 {
				out, i = append(out, factoryd.ChatConversation{Login: login, Number: m.Conversation, FirstAt: m.At}), len(out)
			}
			out[i].LastAt = m.At
			out[i].Messages++
		}
	}
	slices.SortFunc(out, func(a, b factoryd.ChatConversation) int { return cmp.Compare(b.Number, a.Number) })
	return out, nil
}

func (f *acceptChat) ChatMessages(_ context.Context, repository, login string, conversation int64, limit int) ([]factoryd.ChatMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if conversation == 0 {
		conversation = f.current(repository, login)
	}
	var out []factoryd.ChatMessage
	for _, m := range f.msgs[repository+" "+login] {
		if m.Conversation == conversation {
			out = append(out, m)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (f *acceptChat) ProposalStates(_ context.Context, repository, login string, tokens []string, now time.Time) ([]factoryd.ProposalState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]factoryd.ProposalState, len(tokens))
	for i, tok := range tokens {
		out[i] = factoryd.ProposalState{Token: tok, State: "expired"}
		for _, p := range f.proposals {
			switch {
			case p.Kind != "proposal" || p.Token != tok || p.Repository != repository || p.Login != login:
			case !p.used.IsZero():
				out[i].State = "used"
			case !p.ExpiresAt.Before(now):
				out[i].State = "pending"
			}
		}
	}
	return out, nil
}

func (f *acceptChat) ProjectMonthSpend(_ context.Context, _ string, _ time.Time) (factoryd.ProjectSpend, error) {
	return factoryd.ProjectSpend{}, nil
}

func (f *acceptChat) PutProposal(_ context.Context, p factoryd.ChatProposal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proposals = append(f.proposals, acceptProposal{ChatProposal: p})
	return nil
}

func (f *acceptChat) TakeProposal(_ context.Context, kind, token, repository, login string, now time.Time) (factoryd.ChatProposal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, p := range f.proposals {
		if p.Kind != kind || p.Token != token || p.Repository != repository || !p.used.IsZero() || now.After(p.ExpiresAt) {
			continue
		}
		if p.Login != login {
			return factoryd.ChatProposal{}, factoryd.ErrOthersProposal
		}
		p.used = now
		f.proposals[i] = p
		return p.ChatProposal, nil
	}
	return factoryd.ChatProposal{}, factoryd.ErrNoProposal
}

func (f *acceptChat) PurgeProposals(_ context.Context, before time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	f.proposals = slices.DeleteFunc(f.proposals, func(p acceptProposal) bool {
		drop := p.ExpiresAt.Before(before) && (p.used.IsZero() || p.Kind == "undo")
		if drop {
			n++
		}
		return drop
	})
	return n, nil
}

// acceptForgejo is the sign-in's provider: it hands out tokens and answers
// only who the bearer of one is, which is what the login reads.
type acceptForgejo struct {
	mu        sync.Mutex
	t         *testing.T
	challenge string
	user      string
	tokenN    int
}

func (f *acceptForgejo) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/login/oauth/access_token" {
		if err := r.ParseForm(); err != nil {
			f.t.Errorf("token form: %v", err)
		}
		if r.PostForm.Get("grant_type") == "authorization_code" {
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
				http.Error(w, `{"error":"failed PKCE code challenge"}`, http.StatusBadRequest)
				return
			}
		}
		f.tokenN++
		json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("access-%d", f.tokenN),
			"refresh_token": fmt.Sprintf("refresh-%d", f.tokenN), "token_type": "bearer", "expires_in": 3600})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/v1/user" {
		json.NewEncoder(w).Encode(map[string]any{"login": f.user})
		return
	}
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/repos/") && strings.HasSuffix(r.URL.Path, "/labels") {
		if page := r.URL.Query().Get("page"); page != "" && page != "1" {
			json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "name": "backlog"},
			{"id": 2, "name": "plan"},
			{"id": 3, "name": "agent"},
		})
		return
	}
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/repos/") && strings.HasSuffix(r.URL.Path, "/labels") {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]map[string]any{})
		return
	}
	http.Error(w, "no such call: "+r.URL.Path, http.StatusNotFound)
}

// gateway scripts the operator model's replies: each round is one reply
// of streamed chunks, tool calls or text, as the OpenAI stream answers.
type gateway struct {
	t        *testing.T
	mu       sync.Mutex
	replies  [][]string
	requests []map[string]any
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	// a naming call (roadmap 304) answers nothing and consumes no reply:
	// today's head stands
	if r.Header.Get(gwpack.HeaderFactoryPurpose) == policy.PurposeTitle {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", say(""))
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		g.t.Errorf("model call body: %v", err)
	}
	g.requests = append(g.requests, body)
	if len(g.replies) == 0 {
		g.t.Errorf("an unscripted model call")
		http.Error(w, "unscripted", http.StatusInternalServerError)
		return
	}
	reply := g.replies[0]
	g.replies = g.replies[1:]
	if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer gw-token" {
		g.t.Errorf("model call %s: want the operator endpoint with the factoryd token", r.URL.Path)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, chunk := range reply {
		fmt.Fprintf(w, "data: %s\n\n", chunk)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func say(s string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": s}}}})
	return string(b)
}

func call(first int, nameArgs ...string) string {
	var calls []any
	for i := 0; i < len(nameArgs); i += 2 {
		calls = append(calls, map[string]any{"index": i / 2, "id": fmt.Sprintf("call_%d", first+i/2), "type": "function",
			"function": map[string]any{"name": nameArgs[i], "arguments": nameArgs[i+1]}})
	}
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": calls}}}})
	return string(b)
}

// rig is the console over the fixture stores with a logged-in writer of
// the factory's own code and the chat's gateway scripted.
type rig struct {
	c       *console.Console
	srv     *httptest.Server
	ledger  *acceptLedger
	chat    *acceptChat
	gateway *gateway
	ck      *http.Cookie
	csrf    string
}

const chatPath = "/projects/ai-software-factory-code/chat"

func newRig(t *testing.T) *rig {
	t.Helper()
	fg := &acceptForgejo{t: t, user: "sebastian"}
	fgsrv := httptest.NewServer(http.HandlerFunc(fg.serve))
	t.Cleanup(fgsrv.Close)
	led := &acceptLedger{}
	tasks := &acceptTasks{tasks: []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-m", Kind: "agent", Status: "backlog", Lane: policy.LaneBacklog, Process: "maintenance",
			Repository: "sf/code", IssueNumber: 5, Objective: "Fix the cache.\n\nAcceptance:\n- go test ./internal/cache/", BudgetEUR: 3,
			Size: "small"}},
		{Task: factoryd.Task{ID: "t-r", Kind: "agent", Status: "running", Gate: factoryd.GateSession, Lane: policy.LaneReady,
			Process: "maintenance", Repository: "sf/code", IssueNumber: 11, PRNumber: 9, Objective: "Speed up the board."}},
	}}
	evs := &acceptEvents{bus: factoryd.NewEventBus(), tasks: tasks}
	ch := newAcceptChat(tasks)
	gw := &gateway{t: t}
	gwsrv := httptest.NewServer(gw)
	t.Cleanup(gwsrv.Close)

	c, err := console.New(console.Config{
		PublicURL: "https://console.example", ForgejoURL: fgsrv.URL, ForgejoPublicURL: fgsrv.URL,
		ClientID: "cid", ClientSecret: "csecret", SessionKey: []byte("0123456789abcdef0123456789abcdef"),
		Sessions: newAcceptSessions(), Tasks: tasks, Timelines: acceptObjects{}, Policies: led, Costs: led,
		Writers: func(context.Context) (string, map[string][]string, error) {
			return "sf/code", map[string][]string{"sf/code": {"sebastian"}}, nil
		},
		ConfigRepo: "https://git.example/sf/infra", ConfigDir: "factory",
		Approvals: &acceptApprovals{}, Resources: &acceptResources{}, Alerts: &acceptAlerts{},
		Projects: &acceptProjects{rows: []factoryd.Project{{Name: "ai-software-factory-code", Repository: "sf/code",
			MergeRule: "human", Objects: policy.ObjectsSeed, Agents: "coder", File: true, Process: "maintenance",
			Lanes: []policy.Lane{{Name: "backlog", WriteUp: "analyzer"}, {Name: "ready", Class: "coder", MovedBy: "writers"}}}}},
		Events: evs, Bus: evs.bus, Chat: ch,
		Mirrors:  &acceptMirrors{rows: map[string]factoryd.Mirror{}, push: map[string][]factoryd.PushMirror{}},
		Operator: &factoryd.OperatorModel{GatewayURL: gwsrv.URL, Token: "gw-token"},
		MayMove:  func(_ context.Context, _, user, _ string) (bool, error) { return user == "sebastian", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{c: c, srv: httptest.NewServer(c), ledger: led, chat: ch, gateway: gw}
	t.Cleanup(r.srv.Close)
	r.login(t, fg)
	return r
}

// login runs the console's own sign-in as the provider would answer it:
// /login, the callback with the code, and the session cookie and CSRF
// token of the writer of the factory's own code who logs in.
func (r *rig) login(t *testing.T, fg *acceptForgejo) {
	t.Helper()
	rec := r.get("/login")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("/login = %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	fg.mu.Lock()
	fg.challenge = loc.Query().Get("code_challenge")
	fg.mu.Unlock()
	state := loc.Query().Get("state")
	cb := r.get("/auth/callback?code=the-code&state="+url.QueryEscape(state), cookieOf(rec, "__Host-console-login"))
	ck := cookieOf(cb, "__Host-console")
	if ck == nil || cb.Code != http.StatusSeeOther {
		t.Fatalf("the callback = %d, session cookie %v", cb.Code, ck)
	}
	r.ck = ck
	r.csrf = r.csrfOf(t, ck)
}

// csrfOf is the session's form token, read from the page it signs.
func (r *rig) csrfOf(t *testing.T, ck *http.Cookie) string {
	t.Helper()
	rec := r.get("/", ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("the board after the login = %d: %s", rec.Code, rec.Body)
	}
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindSubmatch(rec.Body.Bytes())
	if m == nil {
		t.Fatalf("no csrf token on the board page")
	}
	return string(m[1])
}

func (r *rig) get(path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req, err := http.NewRequest(http.MethodGet, r.srv.URL+path, nil)
	if err != nil {
		panic(err)
	}
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	r.srv.Config.Handler.ServeHTTP(rec, req)
	return rec
}

func cookieOf(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == name {
			return ck
		}
	}
	return nil
}

// post sends the chat message as the logged-in member and reads back the
// stream's events as "event data".
func (r *rig) post(message string) (int, []string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("message", message)
	w.WriteField("csrf", r.csrf)
	w.Close()
	req, err := http.NewRequest(http.MethodPost, r.srv.URL+chatPath, &buf)
	if err != nil {
		panic(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(r.ck)
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		return resp.StatusCode, []string{strings.TrimSpace(string(body))}
	}
	var events []string
	for _, msg := range strings.Split(strings.TrimSuffix(string(body), "\n\n"), "\n\n") {
		lines := strings.Split(msg, "\n")
		event := strings.TrimPrefix(lines[0], "event: ")
		data := ""
		if len(lines) > 1 {
			data = strings.TrimPrefix(lines[1], "data: ")
		}
		events = append(events, event+" "+data)
	}
	return resp.StatusCode, events
}

func equal(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s =\n%#v\nwant\n%#v", what, got, want)
	}
}

// TestCostsSpendPerCardToday asks the agent, through the chat, for the
// spend per card today; the costs tool answers with that day's card sum
// matching the fixture's hand-derived total for 2026-10-11: t-r 1.5000
// and t-m 0.8000, total 2.3000 EUR.
func TestCostsSpendPerCardToday(t *testing.T) {
	r := newRig(t)
	r.gateway.replies = [][]string{
		{call(1, "costs", `{"by":"card","from_date":"2026-10-11","to_date":"2026-10-11"}`)},
		{say("Today: t-r 1.5000, t-m 0.8000, 2.3000 EUR.")},
	}
	code, events := r.post("spend per card today")
	if code != http.StatusOK {
		t.Fatalf("chat = %d %v", code, events)
	}
	if !hasToolResult(events, "costs", daySumCard) {
		t.Fatalf("the costs tool did not answer today's hand-derived card sum; events:\n%s", strings.Join(events, "\n"))
	}
	equal(t, "the ledger's window", r.ledger.windows(), [][2]string{{"2026-10-11", "2026-10-11"}})
}

// TestCostsCustomWindowSums asks the costs tool with from='2026-10-10',
// to='2026-10-11': the aggregated sum matches the hand-derived total
// 2.3000 EUR (0.6 + 1.7), and the ledger was asked exactly that window.
func TestCostsCustomWindowSums(t *testing.T) {
	r := newRig(t)
	r.gateway.replies = [][]string{
		{call(1, "costs", `{"by":"day","from_date":"2026-10-10","to_date":"2026-10-11"}`)},
		{say("Both days together: 2.3000 EUR.")},
	}
	code, events := r.post("spend 2026-10-10 to 2026-10-11")
	if code != http.StatusOK {
		t.Fatalf("chat = %d %v", code, events)
	}
	if !hasToolResult(events, "costs", windowSumDay) {
		t.Fatalf("the costs tool did not answer the window's hand-derived total; events:\n%s", strings.Join(events, "\n"))
	}
	equal(t, "the ledger's window", r.ledger.windows(), [][2]string{{"2026-10-10", "2026-10-11"}})
}

// TestCostsDefaultKeepsCurrentMonth: with no dates the costs read still
// asks the current UTC month, its first day to today.
func TestCostsDefaultKeepsCurrentMonth(t *testing.T) {
	r := newRig(t)
	r.gateway.replies = [][]string{
		{call(1, "costs", `{"by":"day"}`)},
		{say("The month so far.")},
	}
	code, events := r.post("this month's spend")
	if code != http.StatusOK {
		t.Fatalf("chat = %d %v", code, events)
	}
	if !hasToolCall(events, "costs") {
		t.Fatalf("the costs tool was not called; events:\n%s", strings.Join(events, "\n"))
	}
	want := [][2]string{{time.Now().UTC().Format("2006-01") + "-01", time.Now().UTC().Format(policy.DateLayout)}}
	equal(t, "the ledger's window", r.ledger.windows(), want)
}

// TestCostsInvalidDatesRefused: a malformed or reversed window is refused
// with 400 in the tool's words; the ledger is not asked at all.
func TestCostsInvalidDatesRefused(t *testing.T) {
	r := newRig(t)
	r.gateway.replies = [][]string{
		{call(1, "costs", `{"by":"day","from_date":"2026-10-11","to_date":"last tuesday"}`,
			"costs", `{"by":"card","from_date":"20261011"}`,
			"costs", `{"by":"tier","from_date":"2026-02-30"}`,
			"costs", `{"by":"day","from_date":"2026-10-11","to_date":"2026-10-10"}`)},
		{say("All four refused.")},
	}
	code, events := r.post("bad dates please")
	if code != http.StatusOK {
		t.Fatalf("chat = %d %v", code, events)
	}
	n := 0
	for _, e := range events {
		if strings.HasPrefix(e, "tool ") && strings.Contains(e, `"name":"costs"`) {
			n++
			if !strings.Contains(e, badWindowRefusal) {
				t.Fatalf("a bad window was not refused in the tool's words: %s", e)
			}
		}
	}
	if n != 4 {
		t.Fatalf("%d costs results, want 4; events:\n%s", n, strings.Join(events, "\n"))
	}
	if got := r.ledger.windows(); len(got) != 0 {
		t.Fatalf("the ledger was asked for %+v though every window was invalid", got)
	}
}

func hasToolResult(events []string, tool, want string) bool {
	for _, e := range events {
		if !strings.HasPrefix(e, "tool ") {
			continue
		}
		var payload struct {
			Name   string `json:"name"`
			Result string `json:"result"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(e, "tool ")), &payload); err != nil {
			continue
		}
		if payload.Name == tool && payload.Result == want {
			return true
		}
	}
	return false
}

func hasToolCall(events []string, tool string) bool {
	for _, e := range events {
		if strings.HasPrefix(e, "tool ") && strings.Contains(e, `"name":"`+tool+`"`) {
			return true
		}
	}
	return false
}

func (f *acceptChat) SaveSummary(context.Context, string, string, int64, factoryd.ThreadSummary) error {
	return nil
}

func (f *acceptChat) ThreadTokens(context.Context, string, time.Time) (int, error) { return 0, nil }

func (f *acceptChat) ThreadTotals(context.Context, string) (int, float64, error) { return 0, 0, nil }
