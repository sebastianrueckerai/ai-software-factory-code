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

// Policies reads the policy version being enforced (policy_versions).
type Policies interface {
	CurrentPolicy(ctx context.Context) (policy.Version, bool, error)
}

// Costs reads the ledger between two UTC days, both included.
type Costs interface {
	Costs(ctx context.Context, from, to time.Time) (policy.Costs, error)
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
	// ProjectByRepository is the project whose file names repository
	// (owner/name); false when none does.
	ProjectByRepository(ctx context.Context, repository string) (factoryd.Project, bool, error)
}

// Events is the event spine (factoryd.Events): one row per action,
// whatever the input, and a project's feed.
type Events interface {
	factoryd.Events
}
