// Package acceptance holds the plan's acceptance tests. This file is the
// live check of "every plan answer in one turn" (roadmap 179, Stage 18
// item 3.7), card batch-answer-acceptance of plan #506 (3 of 3): a plan
// with three open questions is filed; one chat message answering all
// three lands as exactly one answer tool call — one turn — stores all
// three answers, posts all three comments on the issue, and resolves the
// plan's approval as superseded.
package acceptance

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/console"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The fixture plan: card t-p of issue 506, revision 3 with three open
// questions, and its pending plan approval 42 at that revision.
const (
	planTask   = "t-p"
	planIssue  = int64(506)
	planRev    = 3
	planSHA    = "a000000000000000000000000000000000000000000000000000000000000000"
	planApprID = int64(42)
)

var planFixtureQuestions = []factoryd.PlanQuestion{
	{Text: "Which timeout?", Choices: []string{"60s", "120s"}},
	{Text: "Where do retries live?", Choices: []string{"runner", "gateway"}},
	{Text: "Keep the old flag?", Choices: []string{"yes", "no"}},
}

var planWantAnswers = map[int]string{1: "60 seconds", 2: "in the runner", 3: "keep it"}

// accBatchTasks are the board's rows plus the fixture plan: its revision
// and its questions with their stored answers. answerBatch is the
// store-side batch semantics factoryd.AnswerPlanQuestions stands on:
// every entry must name an open question of the revision or none is
// stored.
type accBatchTasks struct {
	mu        sync.Mutex
	tasks     []factoryd.BoardTask
	plans     map[string][]factoryd.PlanRevision
	questions map[string][]factoryd.PlanQuestion
}

func newAccBatchTasks() *accBatchTasks {
	qs := make([]factoryd.PlanQuestion, len(planFixtureQuestions))
	copy(qs, planFixtureQuestions)
	return &accBatchTasks{
		tasks: []factoryd.BoardTask{
			{Task: factoryd.Task{ID: planTask, Kind: "agent", Status: "waiting-human", Lane: policy.LanePlan,
				Process: "maintenance", Repository: "sf/code", IssueNumber: planIssue,
				Objective: "Every plan answer in one turn."}},
		},
		plans:     map[string][]factoryd.PlanRevision{planTask: {{TaskID: planTask, Revision: planRev, SHA256: planSHA, Plan: json.RawMessage(`{"summary":"Answer every plan question in one turn.","cards":[{"key":"batch-answer-store","role":"foundation","title":"Store the batch answer"},{"key":"answer-tool-batch","role":"integration","title":"Answer all questions in one call"}]}`)}}},
		questions: map[string][]factoryd.PlanQuestion{fmt.Sprintf("%s %d", planTask, planRev): qs},
	}
}

func (s *accBatchTasks) Task(_ context.Context, id string) (factoryd.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tasks {
		if t.ID == id {
			return t.Task, true, nil
		}
	}
	return factoryd.Task{}, false, nil
}

func (s *accBatchTasks) TaskByPR(_ context.Context, repository string, pr int64) (factoryd.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, bt := range s.tasks {
		if bt.Task.Repository == repository && bt.Task.PRNumber == pr && pr != 0 {
			return bt.Task, true, nil
		}
	}
	return factoryd.Task{}, false, nil
}

func (s *accBatchTasks) BoardTasks(_ context.Context, _ time.Time, limit int) ([]factoryd.BoardTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.tasks)
	return out[:min(limit, len(out))], nil
}

func (s *accBatchTasks) BoardTask(_ context.Context, id string) (factoryd.BoardTask, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.tasks, func(t factoryd.BoardTask) bool { return t.ID == id })
	if i < 0 {
		return factoryd.BoardTask{}, false, nil
	}
	return s.tasks[i], true, nil
}

func (s *accBatchTasks) PlanRevision(_ context.Context, id string, n int) (factoryd.PlanRevision, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	revs := s.plans[id]
	for i := len(revs) - 1; i >= 0; i-- {
		if n == 0 || revs[i].Revision == n {
			return revs[i], true, nil
		}
	}
	return factoryd.PlanRevision{}, false, nil
}

func (s *accBatchTasks) PlanQuestions(_ context.Context, id string, revision int) ([]factoryd.PlanQuestion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.questions[fmt.Sprintf("%s %d", id, revision)]), nil
}

func (s *accBatchTasks) PlanCards(_ context.Context, _ string) ([]factoryd.Task, error) {
	return nil, nil
}

func (s *accBatchTasks) Dependencies(_ context.Context, _ string) ([]factoryd.Dependency, error) {
	return nil, nil
}

func (s *accBatchTasks) Dependents(_ context.Context, _ string) ([]factoryd.Dependency, error) {
	return nil, nil
}

func (s *accBatchTasks) SetHumanSummary(_ context.Context, _, _ string) error { return nil }
func (s *accBatchTasks) SetTaskAccent(_ context.Context, _, _ string) error   { return nil }
func (s *accBatchTasks) SetObjective(_ context.Context, _, _ string) error    { return nil }
func (s *accBatchTasks) RaiseTaskBudget(_ context.Context, _ string, _ float64) (float64, float64, error) {
	return 0, 0, nil
}
func (s *accBatchTasks) AddTaskDeps(_ context.Context, _ string, _ []string) error {
	return nil
}
func (s *accBatchTasks) RemoveTaskDeps(_ context.Context, _ string, _ []string) error {
	return nil
}

func (s *accBatchTasks) answerBatch(id string, revision int, answers []factoryd.PlanAnswer, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%s %d", id, revision)
	qs := s.questions[key]
	for _, a := range answers {
		if a.Idx < 1 || a.Idx > len(qs) {
			return factoryd.ErrNotFound
		}
		if qs[a.Idx-1].Answer != "" {
			return factoryd.ErrAnswered
		}
	}
	for _, a := range answers {
		qs[a.Idx-1].Answer, qs[a.Idx-1].AnsweredBy = a.Text, by
	}
	s.questions[key] = qs
	return nil
}

func (s *accBatchTasks) answeredByIndex(idx int) (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.questions[fmt.Sprintf("%s %d", planTask, planRev)][idx-1]
	return q.Answer, q.AnsweredBy
}

func (s *accBatchTasks) openCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, q := range s.questions[fmt.Sprintf("%s %d", planTask, planRev)] {
		if q.Answer == "" {
			n++
		}
	}
	return n
}

// accBatchApprovals are the approval_requests rows in memory;
// ResolveApprovals is factoryd.PGStore.ResolveApprovals on them.
type accBatchApprovals struct {
	mu   sync.Mutex
	rows []factoryd.ApprovalRequest
}

func (f *accBatchApprovals) ApprovalRequests(_ context.Context, status string) ([]factoryd.ApprovalRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []factoryd.ApprovalRequest
	for _, r := range f.rows {
		if r.Status == status {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *accBatchApprovals) DecideApproval(_ context.Context, id int64, kind, status, by, note string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.rows {
		if r.ID == id && r.Kind == kind && r.Status == "pending" {
			f.rows[i].Status, f.rows[i].DecidedBy, f.rows[i].DecisionNote = status, by, note
			now := time.Now()
			f.rows[i].DecidedAt = &now
			return nil
		}
	}
	return factoryd.ErrNotFound
}

func (f *accBatchApprovals) ResolveApprovals(_ context.Context, taskID, kind, status, by, note string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var hit bool
	for i, r := range f.rows {
		if r.TaskID == taskID && r.Kind == kind && r.Status == "pending" {
			f.rows[i].Status, f.rows[i].DecidedBy, f.rows[i].DecisionNote = status, by, note
			hit = true
		}
	}
	if !hit {
		return factoryd.ErrNotFound
	}
	return nil
}

func (f *accBatchApprovals) row(id int64) factoryd.ApprovalRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.ID == id {
			return r
		}
	}
	return factoryd.ApprovalRequest{}
}

// accBatchStore is factoryd's Store seam for the plan business logic: the
// fixture's task rows, plan questions and approval requests.
type accBatchStore struct {
	tasks     *accBatchTasks
	approvals *accBatchApprovals
}

func (s *accBatchStore) Task(ctx context.Context, id string) (factoryd.Task, bool, error) {
	return s.tasks.Task(ctx, id)
}

func (s *accBatchStore) PlanRevision(ctx context.Context, id string, n int) (factoryd.PlanRevision, bool, error) {
	return s.tasks.PlanRevision(ctx, id, n)
}

func (s *accBatchStore) PlanQuestions(ctx context.Context, id string, revision int) ([]factoryd.PlanQuestion, error) {
	return s.tasks.PlanQuestions(ctx, id, revision)
}

func (s *accBatchStore) OpenPlanQuestions(ctx context.Context, id string, revision int) (int, error) {
	qs, err := s.tasks.PlanQuestions(ctx, id, revision)
	n := 0
	for _, q := range qs {
		if q.Answer == "" {
			n++
		}
	}
	return n, err
}

func (s *accBatchStore) AnswerPlanQuestions(ctx context.Context, id string, revision int, answers []factoryd.PlanAnswer, by string) error {
	return s.tasks.answerBatch(id, revision, answers, by)
}

func (s *accBatchStore) ApprovalRequests(ctx context.Context, status string) ([]factoryd.ApprovalRequest, error) {
	return s.approvals.ApprovalRequests(ctx, status)
}

func (s *accBatchStore) ResolveApprovals(ctx context.Context, taskID, kind, status, by, note string) error {
	return s.approvals.ResolveApprovals(ctx, taskID, kind, status, by, note)
}

// The rest of factoryd's Store: the fixture plan flow touches none of it.
func (s *accBatchStore) CreateTask(ctx context.Context, t factoryd.Task) error { panic("unused") }
func (s *accBatchStore) TransitionTask(ctx context.Context, id string, from []string, to string) error {
	panic("unused")
}
func (s *accBatchStore) MarkStarted(ctx context.Context, id string) error { panic("unused") }
func (s *accBatchStore) ListTasksByStatus(ctx context.Context, status string) ([]factoryd.Task, error) {
	panic("unused")
}
func (s *accBatchStore) RecentlyFinishedTasks(ctx context.Context, within time.Duration) ([]string, error) {
	panic("unused")
}
func (s *accBatchStore) SettleSpend(ctx context.Context, id string, reported float64) (bool, error) {
	panic("unused")
}
func (s *accBatchStore) UnsettledTasks(ctx context.Context, olderThan time.Duration) ([]string, error) {
	panic("unused")
}
func (s *accBatchStore) SettleSpendFinal(ctx context.Context, id string, reported float64) (bool, error) {
	panic("unused")
}
func (s *accBatchStore) TaskByIssue(ctx context.Context, repository string, issueNumber int64) (factoryd.Task, bool, error) {
	panic("unused")
}
func (s *accBatchStore) TaskByPR(ctx context.Context, repository string, pr int64) (factoryd.Task, bool, error) {
	return s.tasks.TaskByPR(ctx, repository, pr)
}
func (s *accBatchStore) InsertIntakeEvent(ctx context.Context, e factoryd.IntakeEvent) (int64, error) {
	panic("unused")
}
func (s *accBatchStore) IntakeEvents(ctx context.Context, taskID string) ([]factoryd.IntakeEvent, error) {
	panic("unused")
}
func (s *accBatchStore) SetSignalled(ctx context.Context, id int64, signalled bool) (bool, error) {
	panic("unused")
}
func (s *accBatchStore) UnaddressedChangeRequests(ctx context.Context, taskID string) ([]factoryd.IntakeEvent, error) {
	panic("unused")
}
func (s *accBatchStore) SetTaskPR(ctx context.Context, id string, number int64) error {
	panic("unused")
}
func (s *accBatchStore) SetAgentSummary(ctx context.Context, id, summary string) error {
	panic("unused")
}
func (s *accBatchStore) MonthCapRefusal(ctx context.Context, taskID string, since time.Time) (*policy.Refusal, error) {
	panic("unused")
}
func (s *accBatchStore) ProviderRefusal(ctx context.Context, taskID string, since time.Time) (reason string, ok bool, err error) {
	panic("unused")
}
func (s *accBatchStore) SetRollHold(ctx context.Context, taskID string, since, until time.Time) error {
	panic("unused")
}
func (s *accBatchStore) ClearRollHold(ctx context.Context, taskID string) error { panic("unused") }
func (s *accBatchStore) EnterLane(ctx context.Context, id, lane, class, sha string) (bool, error) {
	panic("unused")
}
func (s *accBatchStore) ToHumanLane(ctx context.Context, id string) (bool, error) { panic("unused") }
func (s *accBatchStore) ProjectByRepository(ctx context.Context, repository string) (factoryd.Project, bool, error) {
	panic("unused")
}
func (s *accBatchStore) SetTaskUsage(ctx context.Context, id string, u factoryd.TaskUsage) error {
	panic("unused")
}
func (s *accBatchStore) DeliverySeen(ctx context.Context, id string) (bool, error) { panic("unused") }
func (s *accBatchStore) RecordDelivery(ctx context.Context, id string) error       { panic("unused") }
func (s *accBatchStore) PruneDeliveries(ctx context.Context, age time.Duration) error {
	panic("unused")
}
func (s *accBatchStore) InsertSession(ctx context.Context, sess factoryd.Session) error {
	panic("unused")
}
func (s *accBatchStore) Session(ctx context.Context, id string) (factoryd.Session, bool, error) {
	panic("unused")
}
func (s *accBatchStore) UpdateSession(ctx context.Context, id string, findings []string, restartState []byte) error {
	panic("unused")
}
func (s *accBatchStore) SetTaskRisk(ctx context.Context, id, class string) error { panic("unused") }
func (s *accBatchStore) SetTaskAlertFingerprint(ctx context.Context, id, fingerprint string) error {
	panic("unused")
}
func (s *accBatchStore) OpenApproval(ctx context.Context, a factoryd.ApprovalRequest) (int64, error) {
	panic("unused")
}
func (s *accBatchStore) DecideApproval(ctx context.Context, id int64, kind, status, by, note string) error {
	panic("unused")
}
func (s *accBatchStore) UndecideApproval(ctx context.Context, id int64) error { panic("unused") }
func (s *accBatchStore) ReopenApproval(ctx context.Context, taskID, kind, note string) error {
	panic("unused")
}
func (s *accBatchStore) SetGate(ctx context.Context, id, gate string, since time.Time) error {
	panic("unused")
}
func (s *accBatchStore) InsertAlert(ctx context.Context, a factoryd.Alert) (int64, error) {
	panic("unused")
}
func (s *accBatchStore) ResolveAlert(ctx context.Context, id int64, kind, verdict, by string, detail map[string]any) error {
	panic("unused")
}
func (s *accBatchStore) JudgeTaskAlerts(ctx context.Context, taskID, kind, verdict, by string) error {
	panic("unused")
}
func (s *accBatchStore) OpenReportTask(ctx context.Context, t factoryd.Task) (factoryd.Task, error) {
	panic("unused")
}
func (s *accBatchStore) ReportOf(ctx context.Context, parent, source string) (factoryd.Task, bool, error) {
	panic("unused")
}
func (s *accBatchStore) PlanCards(ctx context.Context, plan string) ([]factoryd.Task, error) {
	panic("unused")
}
func (s *accBatchStore) AddTaskDeps(ctx context.Context, id string, dependsOn []string) error {
	panic("unused")
}
func (s *accBatchStore) OpenDependencies(ctx context.Context, id string) ([]factoryd.Dependency, error) {
	panic("unused")
}
func (s *accBatchStore) WaitingDependents(ctx context.Context, id string) ([]string, error) {
	panic("unused")
}
func (s *accBatchStore) ReplaceFailedCard(ctx context.Context, id string, t factoryd.Task) (bool, error) {
	panic("unused")
}
func (s *accBatchStore) AddPlanRevision(ctx context.Context, id string, plan json.RawMessage, sha string) (int, error) {
	panic("unused")
}
func (s *accBatchStore) PutPlanQuestions(ctx context.Context, id string, revision int, qs []factoryd.PlanQuestion) (bool, error) {
	panic("unused")
}

// AnswerPlanQuestion is factoryd.PGStore.AnswerPlanQuestion on the fixture's // rows: an unknown number answers ErrNotFound, an answered one ErrAnswered.
func (s *accBatchStore) AnswerPlanQuestion(ctx context.Context, id string, revision, idx int, answer, by string) error {
	return s.tasks.answerBatch(id, revision, []factoryd.PlanAnswer{{Idx: idx, Text: answer}}, by)
}
func (s *accBatchStore) CreatePlanCard(ctx context.Context, t factoryd.Task) (factoryd.Task, error) {
	panic("unused")
}
func (s *accBatchStore) SetTaskIssue(ctx context.Context, id string, issue int64) error {
	panic("unused")
}
func (s *accBatchStore) UncommittedRolls(ctx context.Context, taskID string) ([]factoryd.RollRequest, error) {
	panic("unused")
}
func (s *accBatchStore) MarkRollsCommitted(ctx context.Context, taskID string, shas []string, head string) error {
	panic("unused")
}
func (s *accBatchStore) RollHead(ctx context.Context, taskID string) (string, error) { panic("unused") }
func (s *accBatchStore) SetTaskSHA(ctx context.Context, id, sha string) error        { panic("unused") }
func (s *accBatchStore) EnterQueue(ctx context.Context, project, taskID string) (factoryd.QueueSpot, error) {
	panic("unused")
}
func (s *accBatchStore) QueueHead(ctx context.Context, project string) (string, bool, error) {
	panic("unused")
}
func (s *accBatchStore) LeaveQueue(ctx context.Context, taskID string) (project string, left bool, err error) {
	panic("unused")
}
func (s *accBatchStore) WaitingQueueHeads(ctx context.Context) ([]string, error) { panic("unused") }
func (s *accBatchStore) RollHold(ctx context.Context, now time.Time) (factoryd.RollHold, bool, error) {
	panic("unused")
}

// accBatchPolicies serves the fixture tree as the version in force.
type accBatchPolicies struct{ tree policy.Tree }

func (p accBatchPolicies) CurrentPolicy(context.Context) (policy.Version, bool, error) {
	return policy.Version{ID: 12, Commit: "c12", Tree: p.tree, LoadedAt: time.Now(), ActivatedAt: time.Now()}, true, nil
}

// accBatchChat is the chat store in memory: one conversation, its messages
// and proposals, on the fixture's task rows.
type accBatchChat struct {
	mu        sync.Mutex
	msgs      []factoryd.ChatMessage
	proposals []accBatchProposal
	tasks     *accBatchTasks
}

type accBatchProposal struct {
	factoryd.ChatProposal
	used time.Time
}

func (f *accBatchChat) EnsureChatTask(ctx context.Context, name, repository string, budget float64, at time.Time) (factoryd.Task, error) {
	id := factoryd.ChatTaskID(name, at)
	if t, ok, _ := f.tasks.Task(ctx, id); ok {
		return t, nil
	}
	t := factoryd.Task{ID: id, Kind: factoryd.KindChat, Status: "running", BudgetEUR: budget, Repository: repository}
	f.tasks.mu.Lock()
	f.tasks.tasks = append(f.tasks.tasks, factoryd.BoardTask{Task: t})
	f.tasks.mu.Unlock()
	return t, nil
}

func (f *accBatchChat) AddChatMessage(_ context.Context, _, _ string, m factoryd.ChatMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m.ID = int64(len(f.msgs) + 1)
	m.At = time.Now()
	m.Conversation = 1
	f.msgs = append(f.msgs, m)
	return nil
}

func (f *accBatchChat) ChatMessages(_ context.Context, _, _ string, _ int64, limit int) ([]factoryd.ChatMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	all := slices.Clone(f.msgs)
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}

func (f *accBatchChat) CurrentConversation(context.Context, string, string) (int64, error) {
	return 1, nil
}

func (f *accBatchChat) NewConversation(context.Context, string, string) (int64, error) { return 1, nil }

func (f *accBatchChat) Conversations(context.Context, string, string) ([]factoryd.ChatConversation, error) {
	return nil, nil
}

func (f *accBatchChat) ProposalStates(_ context.Context, _, _ string, tokens []string, now time.Time) ([]factoryd.ProposalState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]factoryd.ProposalState, len(tokens))
	for i, token := range tokens {
		out[i] = factoryd.ProposalState{Token: token, State: "expired"}
		for _, p := range f.proposals {
			if p.Token != token || now.After(p.ExpiresAt) {
				continue
			}
			state := "pending"
			if !p.used.IsZero() {
				state = "used"
			}
			out[i] = factoryd.ProposalState{Token: token, State: state}
		}
	}
	return out, nil
}

func (f *accBatchChat) ProjectMonthSpend(context.Context, string, time.Time) (factoryd.ProjectSpend, error) {
	return factoryd.ProjectSpend{}, nil
}

func (f *accBatchChat) PutProposal(_ context.Context, p factoryd.ChatProposal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proposals = append(f.proposals, accBatchProposal{ChatProposal: p})
	return nil
}

func (f *accBatchChat) TakeProposal(_ context.Context, kind, token, repository, login string, now time.Time) (factoryd.ChatProposal, error) {
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

func (f *accBatchChat) PurgeProposals(_ context.Context, before time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.proposals)
	f.proposals = slices.DeleteFunc(f.proposals, func(p accBatchProposal) bool {
		return p.ExpiresAt.Before(before) && (p.used.IsZero() || p.Kind == "undo")
	})
	return int64(n - len(f.proposals)), nil
}

// batchWorkflows is factoryd's Workflows seam: it records each signal the
// answers send to the plan's workflow.
type batchWorkflows struct{ rig *batchRig }

func (w batchWorkflows) Start(context.Context, string) error { return nil }

func (w batchWorkflows) Signal(_ context.Context, taskID, signal string, sig factoryd.TaskSignal) error {
	w.rig.noteSignal(fmt.Sprintf("%s %s %d %d by %s", signal, taskID, sig.Revision, sig.Index, sig.Author))
	return nil
}

// batchForgejo is the sign-in's provider and the issue tracker: it hands
// out tokens, says who the bearer is, and records every comment body
// posted on an issue.
type batchForgejo struct {
	mu        sync.Mutex
	t         *testing.T
	challenge string
	tokenN    int
	commentN  int64
	comments  []string
}

func (f *batchForgejo) serve(w http.ResponseWriter, r *http.Request) {
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
		json.NewEncoder(w).Encode(map[string]any{"login": "sebastian"})
		return
	}
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/506/comments") {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("decode comment: %v", err)
		}
		f.comments = append(f.comments, body["body"])
		f.commentN++
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": f.commentN})
		return
	}
	http.Error(w, "no such call: "+r.URL.Path, http.StatusNotFound)
}

func (f *batchForgejo) postedComments() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.comments)
}

// batchRig is the console over the fixture plan with the production
// business logic wired: console.New rejects a Config whose plan seams are
// nil, so the rig wraps factoryd's own AnswerPlanQuestions (and siblings)
// over the fixture store — the same functions cmd/factoryd wires — and
// records each workflow signal the answers send. Its policy tree carries
// chat.confirm [], so the agent's answer call runs at once inside the
// member's turn: one message, one turn.
type batchRig struct {
	c       *console.Console
	srv     *httptest.Server
	tasks   *accBatchTasks
	apprs   *accBatchApprovals
	evs     *acceptEvents
	gw      *gateway
	fg      *batchForgejo
	mu      sync.Mutex
	signals []string
	ck      *http.Cookie
	csrf    string
}

func (r *batchRig) noteSignal(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.signals = append(r.signals, s)
}

func (r *batchRig) approvalRow(id int64) factoryd.ApprovalRequest { return r.apprs.row(id) }

func (r *batchRig) eventRows(kind string) []factoryd.Event {
	r.evs.mu.Lock()
	defer r.evs.mu.Unlock()
	var out []factoryd.Event
	for _, e := range r.evs.rows {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// batchPolicyTree is the configuration tree the rig enforces: the code
// project with an empty confirm list, so a write tool call runs at once.
func batchPolicyTree(t *testing.T) policy.Tree {
	t.Helper()
	files := map[string][]byte{
		policy.FilePolicy: []byte(policy.Header(policy.FilePolicy) + "\n" + `version: 1
budget: {task_default_eur: 2, task_max_eur: 5, day_eur: 20, month_eur: 50}
limits: {sessions: 3, agent_time: 10m, supervisor_calls: 3, supervisor_timeout: 6m, stronger_after: 1}
classes:
  coder: {session: code, models: [coding-cheap], tools: {}, review_model: review-frontier, supervisor_model: supervisor}
  planner: {session: plan, models: [plan-cheap], tools: {}, supervisor_model: supervisor}
approvers: [sebastian]
risk:
  classes: [{name: normal, outcome: requires_approval}]
  rules: []
  default: normal
envelope_default: {monthly_eur: 10, server_types: [cx23], max_count: 2, max_lease_hours: 4, volume_max_gb: 20}
chat: {confirm: []}
`),
		policy.ProjectFile(accBatchProject): []byte(policy.Header(policy.ProjectFile(accBatchProject)) + "\n" + `repository: sf/code
merge_rule: human
objects: seed
agents: coder
process: maintenance
tokens: {read: forgejo-ci-registrar, actions: forgejo-ci-registrar, merge: forgejo-merge}
`),
		policy.ProcessFile("maintenance"): []byte(policy.Header(policy.ProcessFile("maintenance")) + "\n" + `lanes:
  - {name: backlog}
  - {name: plan, class: planner, moved_by: writers, plan: {max_cards: 8, foundation: true, integration: true}}
  - {name: ready, class: coder, moved_by: writers}
dependencies: {wait_until: done, on_failed: human}
merge: {queue: none}
stages:
  - {name: checks, on_findings: agent, enabled: true}
  - {name: policy, on_findings: fail, enabled: true}
  - {name: approval, on_findings: human, enabled: true}
  - {name: merge, on_findings: human, enabled: true}
`),
	}
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatalf("the fixture tree: %v", err)
	}
	return tree
}

const accBatchProject = "ai-software-factory-code"

func newBatchRig(t *testing.T) *batchRig {
	t.Helper()
	tree := batchPolicyTree(t)
	tasks := newAccBatchTasks()
	apprs := &accBatchApprovals{rows: []factoryd.ApprovalRequest{
		{ID: planApprID, Kind: factoryd.ApprovalPlan, TaskID: planTask, Project: "sf/code", Status: "pending",
			Spec: json.RawMessage(fmt.Sprintf(`{"revision":%d,"sha256":%q}`, planRev, planSHA))},
	}}
	evs := &acceptEvents{bus: factoryd.NewEventBus()}
	store := &accBatchStore{tasks: tasks, approvals: apprs}
	fg := &batchForgejo{t: t}
	fgsrv := httptest.NewServer(http.HandlerFunc(fg.serve))
	t.Cleanup(fgsrv.Close)
	gw := &gateway{t: t}
	gwsrv := httptest.NewServer(gw)
	t.Cleanup(gwsrv.Close)
	r := &batchRig{tasks: tasks, apprs: apprs, evs: evs, gw: gw, fg: fg}
	wf := batchWorkflows{rig: r}
	c, err := console.New(console.Config{
		PublicURL: "https://console.example", ForgejoURL: fgsrv.URL, ForgejoPublicURL: "https://git.example",
		ClientID: "cid", ClientSecret: "csecret", SessionKey: []byte("0123456789abcdef0123456789abcdef"),
		Sessions: newAcceptSessions(), Tasks: tasks, Timelines: acceptObjects{},
		Policies: accBatchPolicies{tree: tree}, Costs: &acceptLedger{},
		Writers: func(context.Context) (string, map[string][]string, error) {
			return "sf/code", map[string][]string{"sf/code": {"sebastian"}}, nil
		},
		Approvals: apprs, Resources: &acceptResources{}, Alerts: &acceptAlerts{},
		Projects: &acceptProjects{rows: []factoryd.Project{{Name: accBatchProject, Repository: "sf/code",
			MergeRule: "human", Objects: policy.ObjectsSeed, Agents: "coder", File: true, Process: "maintenance",
			Lanes: tree.Processes["maintenance"].Lanes}}},
		Events: evs, Bus: evs.bus, Chat: &accBatchChat{tasks: tasks},
		Operator: &factoryd.OperatorModel{GatewayURL: gwsrv.URL, Token: "gw-token"},
		MayMove: func(_ context.Context, repository, user, _ string) (bool, error) {
			return repository == "sf/code" && user == "sebastian", nil
		},
		AcceptPlan: func(ctx context.Context, taskID string, revision int, sha, by string) error {
			return factoryd.AcceptPlan(ctx, store, wf, taskID, revision, sha, by)
		},
		AnswerPlan: func(ctx context.Context, taskID string, revision, idx int, answer, by string) error {
			return factoryd.AnswerPlan(ctx, store, wf, taskID, revision, idx, answer, by)
		},
		AnswerPlanQuestions: func(ctx context.Context, taskID string, revision int, answers []factoryd.PlanAnswer, by string) error {
			return factoryd.AnswerPlanQuestions(ctx, store, wf, taskID, revision, answers, by)
		},
		RejectPlan: func(_ context.Context, _, _, _ string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	r.c = c
	r.srv = httptest.NewServer(c)
	t.Cleanup(r.srv.Close)
	r.login(t)
	return r
}

// login runs the console's own sign-in as the provider answers it: /login,
// the callback with the code, and the session cookie and CSRF token of the
// project's writer who logs in.
func (r *batchRig) login(t *testing.T) {
	t.Helper()
	rec := r.get("/login")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("/login = %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	r.fg.mu.Lock()
	r.fg.challenge = loc.Query().Get("code_challenge")
	r.fg.mu.Unlock()
	cb := r.get("/auth/callback?code=the-code&state="+url.QueryEscape(loc.Query().Get("state")),
		cookieOf(rec, "__Host-console-login"))
	ck := cookieOf(cb, "__Host-console")
	if ck == nil || cb.Code != http.StatusSeeOther {
		t.Fatalf("the callback = %d, session cookie set: %v", cb.Code, ck != nil)
	}
	page := r.get("/", ck)
	if page.Code != http.StatusOK {
		t.Fatalf("the board after the login = %d: %s", page.Code, page.Body)
	}
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindSubmatch(page.Body.Bytes())
	if m == nil {
		t.Fatalf("no csrf token on the board page")
	}
	r.ck, r.csrf = ck, string(m[1])
}

func (r *batchRig) get(path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
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

// postChat sends one chat message as the member and reads back the
// stream's events as "event data".
func (r *batchRig) postChat(message string) (int, []string) {
	form := url.Values{"message": {message}, "csrf": {r.csrf}}
	req, err := http.NewRequest(http.MethodPost, r.srv.URL+"/projects/"+accBatchProject+"/chat", strings.NewReader(form.Encode()))
	if err != nil {
		panic(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
		data := ""
		if len(lines) > 1 {
			data = strings.TrimPrefix(lines[1], "data: ")
		}
		events = append(events, strings.TrimPrefix(lines[0], "event: ")+" "+data)
	}
	return resp.StatusCode, events
}

// TestOneMessageAnswersAllPlanQuestionsInOneTurn is plan #506's acceptance
// item (card batch-answer-acceptance, 3 of 3): the member's one message
// answering the plan's three questions produces exactly one answer tool
// call carrying all three answers, all three answers are stored, all
// three comments are posted on the issue, and the plan's approval
// resolves as superseded.
func TestOneMessageAnswersAllPlanQuestionsInOneTurn(t *testing.T) {
	r := newBatchRig(t)
	r.gw.replies = [][]string{
		{call(1, "answer", `{"task_id":"t-p","revision":3,"answers":[{"question":1,"text":"60 seconds"},{"question":2,"text":"in the runner"},{"question":3,"text":"keep it"}]}`)},
		{say("All three answers are in.")},
	}
	code, events := r.postChat("Q1: 60 seconds. Q2: in the runner. Q3: keep it.")
	if code != http.StatusOK {
		t.Fatalf("chat = %d: %v", code, events)
	}

	// 1. exactly one answer tool call carried all three answers: one turn.
	n := 0
	for _, e := range events {
		if !strings.HasPrefix(e, "tool ") || !strings.Contains(e, `"name":"answer"`) {
			continue
		}
		n++
		var payload struct {
			Name      string `json:"name"`
			Arguments struct {
				TaskID   string `json:"task_id"`
				Revision int    `json:"revision"`
				Answers  []struct {
					Question int    `json:"question"`
					Text     string `json:"text"`
				} `json:"answers"`
			} `json:"arguments"`
			Result string `json:"result"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(e, "tool ")), &payload); err != nil {
			t.Fatalf("the tool event %q: %v", e, err)
		}
		in := payload.Arguments
		if in.TaskID != planTask || in.Revision != planRev || len(in.Answers) != 3 {
			t.Fatalf("the answer call = %+v, want %s revision %d with 3 answers", in, planTask, planRev)
		}
		for _, a := range in.Answers {
			if planWantAnswers[a.Question] != a.Text {
				t.Errorf("the call's answer %d = %q, want %q", a.Question, a.Text, planWantAnswers[a.Question])
			}
		}
		if !strings.Contains(payload.Result, "Answered") {
			t.Fatalf("the one answer call did not apply: %s", payload.Result)
		}
	}
	if n != 1 {
		t.Fatalf("%d answer tool calls, want exactly one (one turn):\n%s", n, strings.Join(events, "\n"))
	}

	// 2. all three answers are stored, as the member's, and none stays open.
	for idx, want := range planWantAnswers {
		got, by := r.tasks.answeredByIndex(idx)
		if got != want {
			t.Errorf("question %d stored %q, want %q", idx, got, want)
		}
		if by != "sebastian" {
			t.Errorf("question %d answered by %q, want sebastian", idx, by)
		}
	}
	if open := r.tasks.openCount(); open != 0 {
		t.Fatalf("%d questions of revision %d stay open, want 0", open, planRev)
	}

	// 3. all three comments are posted on the plan's issue as the member's,
	// in the production format of factoryd.PlanAnswerComment: "Answer N: <text>".
	comments := r.fg.postedComments()
	for idx, want := range planWantAnswers {
		comment := fmt.Sprintf("Answer %d: %s", idx, want)
		if !slices.Contains(comments, comment) {
			t.Errorf("the issue holds no comment %q; it holds %v", comment, comments)
		}
	}

	// 4. the plan's approval resolved as superseded: the planner revises
	// revision 3 with the answers. Production resolves it with status
	// 'denied' and a decision note starting 'superseded'
	// (factoryd.AnswerPlanQuestions), so both are asserted.
	row := r.approvalRow(planApprID)
	if row.Status != "denied" {
		t.Fatalf("the plan approval = %q, want denied (resolved as superseded)", row.Status)
	}
	if !strings.HasPrefix(row.DecisionNote, "superseded") {
		t.Fatalf("the plan approval's note = %q, want it to start with 'superseded'", row.DecisionNote)
	}
	if row.DecidedBy != factoryd.FactoryDecider {
		t.Fatalf("the plan approval decided by %q, want %q", row.DecidedBy, factoryd.FactoryDecider)
	}

	// 5. the flow's spine: three plan_question_answered event rows, and the
	// plan's workflow signalled once per answer — never one call per
	// question across turns.
	evRows := r.eventRows(factoryd.EventPlanQuestionAnswered)
	if len(evRows) != 3 {
		t.Fatalf("%d plan_question_answered events, want 3: %+v", len(evRows), evRows)
	}
	for _, e := range evRows {
		if e.TaskID != planTask || e.IssueNumber != planIssue || e.Actor != "sebastian" || e.Via != factoryd.ViaChat {
			t.Errorf("the event row = %+v, want %s #%d by sebastian via chat", e, planTask, planIssue)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	signalled := 0
	for _, s := range r.signals {
		if strings.HasPrefix(s, factoryd.SignalPlanAnswered+" ") {
			signalled++
		}
	}
	if signalled != 3 {
		t.Fatalf("%d plan_answered signals, want one per answer: %v", signalled, r.signals)
	}
}
