package console

import (
	"context"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The console's seams to the factory's data: approval requests, the agent
// resource registry and envelopes, the cost ledger and the policy versions.

// Approvals are the approval_requests rows (factoryd.PGStore).
type Approvals interface {
	// ApprovalRequests lists the requests in status, oldest first.
	ApprovalRequests(ctx context.Context, status string) ([]factoryd.ApprovalRequest, error)
	// DecideApproval decides one pending request of kind as by;
	// factoryd.ErrNotFound when it is no longer pending.
	DecideApproval(ctx context.Context, id int64, kind, status, by, note string) error
}

// MergeApprover signals a task's workflow that approver approved its pull
// request (factoryd.ApproveMerge), after the human's review is on it.
type MergeApprover func(ctx context.Context, taskID string, pr int64, approver string) error

// FactoryCloser signals a factory pull request card's workflow that by
// closes it with words (factoryd.CloseFactoryPR).
type FactoryCloser func(ctx context.Context, taskID string, pr int64, by, words string) error

// Resources is the agent project's registry and what each project holds
// against its envelope (cloudres.AgentStore). The gateway's collector acts on what they record.
type Resources interface {
	Commissions(ctx context.Context, since time.Time) ([]cloudres.Resource, error)
	Use(ctx context.Context, repo string) (cloudres.EnvelopeUse, error)
	Standing(ctx context.Context, repo string) (cloudres.StandingUse, error)
	Promote(ctx context.Context, id int64, by string, standing func(repo string) policy.Standing) error
	EndLease(ctx context.Context, id int64, by string) error
	// Commission reads one agent row: the repository a promote or release
	// event belongs to (cloudres.AgentStore).
	Commission(ctx context.Context, id int64) (cloudres.Resource, bool, error)
}

// Jobs are the cluster's CronJobs (factoryd.ClusterJobs), which the
// Resources page lists read-only.
type Jobs interface {
	CronJobs(ctx context.Context) ([]factoryd.CronJobState, error)
}

// Policies reads the policy version being enforced (policy_versions).
type Policies interface {
	CurrentPolicy(ctx context.Context) (policy.Version, bool, error)
}

// Costs reads the ledger: its days between two UTC days, both included,
// and a task's model calls.
type Costs interface {
	Costs(ctx context.Context, from, to time.Time) (policy.Costs, error)
	// DaySpend is the ledger's total of one UTC day, the board's figure
	// against the day cap.
	DaySpend(ctx context.Context, day time.Time) (float64, error)
	ModelCalls(ctx context.Context, taskID string) ([]policy.ModelCall, error)
	// CallTotals sums the tasks' audit call rows: their tokens and the
	// modeled cost and plan credits derived from them at read time
	// (roadmap 206), one query, no stored aggregate.
	CallTotals(ctx context.Context, tasks []string) (map[string]policy.CallTotals, error)
}

// Alerts are the alerts rows (factoryd.PGStore): machine alerts judged by
// the machine, human escalations judged by an approver.
type Alerts interface {
	// Alerts lists the newest limit alerts, newest first.
	Alerts(ctx context.Context, limit int) ([]factoryd.Alert, error)
	// SetAlertVerdict judges an open escalation alert as by;
	// factoryd.ErrNotFound when there is none.
	SetAlertVerdict(ctx context.Context, id int64, verdict, by string) error
	// AlertByID reads one alert row: the task whose repository an alert
	// verdict's event carries (factoryd.PGStore).
	AlertByID(ctx context.Context, id int64) (factoryd.Alert, bool, error)
}

// Projects are the configuration's project files joined with their rows
// (factoryd.PGStore).
type Projects interface {
	Projects(ctx context.Context) ([]factoryd.Project, error)
}

// Events is the event spine (factoryd.Events): one row per action,
// whatever the input, and a project's feed. FiledPR is the infra pull
// request of a project's last filing, its project_filed event's.
type Events interface {
	factoryd.Events
	FiledPR(ctx context.Context, name string) (int64, bool, error)
}

// Chat is the project agent's store (factoryd.PGStore): each project's
// chat task, each member's conversations, their proposals and undos, and a
// project's month spend.
type Chat interface {
	EnsureChatTask(ctx context.Context, name, repository string, budget float64, at time.Time) (factoryd.Task, error)
	AddChatMessage(ctx context.Context, repository, login string, m factoryd.ChatMessage) (int64, error)
	ChatMessages(ctx context.Context, repository, login string, conversation int64, limit int) ([]factoryd.ChatMessage, error)
	// ChatAsks are login's ask rows on repository, every thread's, the
	// settle rule's read for the switcher's badge (the
	// notification-settling concept)
	ChatAsks(ctx context.Context, repository, login string) ([]factoryd.ChatMessage, error)
	CurrentConversation(ctx context.Context, repository, login string) (int64, error)
	NewConversation(ctx context.Context, repository, login string) (int64, error)
	Conversations(ctx context.Context, repository, login string) ([]factoryd.ChatConversation, error)
	// ProjectConversations lists every member's threads on the project
	// with each thread's owner named: threads are project-visible, there
	// are no private ones.
	ProjectConversations(ctx context.Context, repository string) ([]factoryd.ChatConversation, error)
	ProposalStates(ctx context.Context, repository, login string, tokens []string, now time.Time) ([]factoryd.ProposalState, error)
	ProjectMonthSpend(ctx context.Context, repository string, at time.Time) (factoryd.ProjectSpend, error)
	PutProposal(ctx context.Context, p factoryd.ChatProposal) error
	TakeProposal(ctx context.Context, kind, token, repository, login string, now time.Time) (factoryd.ChatProposal, error)
	PurgeProposals(ctx context.Context, before time.Time) (int64, error)
	// the threads (roadmap 239): one thread's row, the switch, the title,
	// the seen marker, and a queued row's landing in its place (263)
	Conversation(ctx context.Context, repository, login string, n int64) (factoryd.ChatConversation, bool, error)
	OpenConversation(ctx context.Context, repository, login string, n int64) error
	RenameConversation(ctx context.Context, repository, login string, n int64, title string) (bool, error)
	MarkSeen(ctx context.Context, repository, login string, n int64) error
	// Clear old sweeps the thread's old rows from the member's view: the
	// marker rides the seen marker's table, the record keeps the rows
	MarkCleared(ctx context.Context, repository, login string, n int64) error
	StartQueued(ctx context.Context, repository, login string, id int64) error
	// the inbox thread, made the first time it is asked for; the archive
	// and its restore; the hard delete, which answers the rows it took
	EnsureInbox(ctx context.Context, repository, login string) (int64, error)
	// the card's own thread (the card door): one per card, the first
	// caller binds it, every later caller gets the same one
	EnsureCardThread(ctx context.Context, repository, login, taskID string) (int64, error)
	CardThreadFor(ctx context.Context, repository, taskID string) (string, int64, bool, error)
	// CardThreads is CardThreadFor over many cards at once: one read of
	// the settle rule's bindings.
	CardThreads(ctx context.Context, repository string, ids []string) (map[string]factoryd.CardThread, error)
	// the thread's own subscriptions (the timers concept): its watches of
	// cards and its one-shot timers, which the Waker wakes it on
	AddChatWatch(ctx context.Context, repository, login string, conversation int64, taskID string) error
	RemoveChatWatch(ctx context.Context, repository, login, taskID string) (bool, error)
	AddChatTimer(ctx context.Context, repository, login string, conversation int64, due time.Time, note, taskID string) error
	ArchiveConversation(ctx context.Context, repository, login string, n int64, archived bool) (bool, error)
	DeleteConversation(ctx context.Context, repository, login string, n int64) (int64, error)
	// the intake switch (the chat header's): a closed intake refuses the
	// wake's own filings, the member's ask still files
	SetChatIntake(ctx context.Context, repository string, closed bool, by string, at time.Time) error
	ChatIntakeClosed(ctx context.Context, repository string) (bool, error)
	// compaction: the summary a thread keeps, its newest call's tokens
	// since a time, and its calls' totals
	SaveSummary(ctx context.Context, repository, login string, n int64, s factoryd.ThreadSummary) error
	ThreadTokens(ctx context.Context, thread string, since time.Time) (int, error)
	ThreadTotals(ctx context.Context, thread string) (int, float64, error)
}

// Notes is the projects' scratchpads (factoryd.PGStore): one per
// project, open to every thread of its chat, steering nothing (roadmap
// 239). PutNotes writes only when the scratchpad still hashes to base.
type Notes interface {
	Notes(ctx context.Context, repository string) (factoryd.Notes, error)
	PutNotes(ctx context.Context, repository, login, content, base string) (bool, error)
}

// Sessions is the console's logins (factoryd.PGStore's console_sessions):
// a row per session cookie, keyed by the SHA-256 hex of the cookie's id,
// so a login survives a restart of factoryd (roadmap 164). The access and
// refresh tokens travel to and from it encrypted by the console itself;
// the store never sees them in clear.
type Sessions interface {
	// SaveConsoleSession stores the row, replacing one of the same hash.
	SaveConsoleSession(ctx context.Context, sess factoryd.ConsoleSession) error
	// ConsoleSessionByHash reads the row of the hash; false when there is
	// none or it expired, an expired row deleted.
	ConsoleSessionByHash(ctx context.Context, hash string) (factoryd.ConsoleSession, bool, error)
	// ConsoleSessionsForUser reads the login's live rows newest first:
	// the wake turn's source of the member's tokens.
	ConsoleSessionsForUser(ctx context.Context, login string) ([]factoryd.ConsoleSession, error)
	// DeleteConsoleSession removes the row of the hash (a logout).
	DeleteConsoleSession(ctx context.Context, hash string) error
	// TouchConsoleSession moves the row's last_seen to the store's now
	// when the row still says seenAt, the value the caller read; false
	// when it did not move. A row whose last_seen already moved since that
	// read is a row seen within the minute, so however many Console values
	// share the table, one minute moves the column at most once.
	TouchConsoleSession(ctx context.Context, hash string, seenAt time.Time) (bool, error)
	// UpdateConsoleSessionTokens stores the refreshed tokens on the row.
	UpdateConsoleSessionTokens(ctx context.Context, hash, access, refresh string, expiry time.Time) error
	// PurgeConsoleSessions deletes the rows expired before at.
	PurgeConsoleSessions(ctx context.Context, at time.Time) (int64, error)
}

// Mirrors are the projects' GitHub exports: their rows (factoryd.PGStore),
// which Mirror reads without the token and MirrorToken, the chat export's
// read, with it, and Forgejo's push mirrors through the projects token
// (factoryd.Forgejo).
type Mirrors interface {
	Mirror(ctx context.Context, project, direction string) (factoryd.Mirror, bool, error)
	MirrorToken(ctx context.Context, project, direction string) (factoryd.Mirror, bool, error)
	PutMirror(ctx context.Context, m factoryd.Mirror) error
	SetImportPR(ctx context.Context, project string, pr int64) error
	DeleteMirror(ctx context.Context, project, direction string) error
	PushMirrors(ctx context.Context, repo factoryd.Repo) ([]factoryd.PushMirror, error)
	AddPushMirror(ctx context.Context, repo factoryd.Repo, address, username, token string) error
	SyncPushMirrors(ctx context.Context, repo factoryd.Repo) error
	DeletePushMirror(ctx context.Context, repo factoryd.Repo, name string) error
	ClearGitHubFailures(ctx context.Context, repository string) error
}

// Routines are the projects' routines (factoryd.Routines, roadmap 267),
// each named by its project's name and its own; a project's board shows
// policy.yaml's template pass too. A routine the project does not run is
// factoryd.ErrNotFound.
type Routines interface {
	// Views are the project's routines as its board shows them.
	Views(ctx context.Context, project string) ([]factoryd.RoutineView, error)
	// Runs are the routine's last limit runs, newest first.
	Runs(ctx context.Context, project, routine string, limit int) ([]factoryd.RoutineRun, error)
	// RunOfTask is the run that filed the task.
	RunOfTask(ctx context.Context, taskID string) (factoryd.RoutineRun, bool, error)
	// Skip sets the routine's Skip once for by, or with skip false
	// clears it.
	Skip(ctx context.Context, project, routine string, skip bool, by string) error
	// RunNow starts a run due now and returns before its outcome.
	RunNow(ctx context.Context, project, routine string) error
}
