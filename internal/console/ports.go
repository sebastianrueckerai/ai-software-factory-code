package console

import (
	"context"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The console's seams to the stage 7 data: approval requests (tasks 53 and
// 55), the resource registry (53), the cost ledger and the policy versions
// (51). factoryd's store implements them.

// Approval is a pending approval_requests row: a merge (task 55) or a
// resource beyond its envelope (task 53).
type Approval struct {
	ID         int64
	Kind       string // "merge" or "resource"
	TaskID     string
	Repository string
	PRNumber   int64  // merge
	Summary    string // the review verdict, or the resource spec and reason
	MonthlyEUR float64
	Created    time.Time
}

// Approvals lists the pending approval requests for the human lane.
type Approvals interface {
	PendingApprovals(ctx context.Context) ([]Approval, error)
}

// Policies reads the policy version being enforced (policy_versions).
type Policies interface {
	CurrentPolicy(ctx context.Context) (policy.Version, bool, error)
}

// Costs reads the ledger between two UTC days, both included.
type Costs interface {
	Costs(ctx context.Context, from, to time.Time) (policy.Costs, error)
}

// Resource is one cloud_resources registry row.
type Resource struct {
	ID          string
	Project     string
	TaskID      string
	Type        string
	IPv4        string
	LeaseExpiry time.Time
	EURPerHour  float64
	CostSoFar   float64
	State       string
}

// Resources lists commissioned resources; promote and release follow in
// the resources slice.
type Resources interface {
	ListResources(ctx context.Context) ([]Resource, error)
}
