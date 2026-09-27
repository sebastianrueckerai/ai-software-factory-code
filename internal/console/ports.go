package console

import (
	"context"
	"encoding/json"
	"time"
)

// The console's seams for the later stage 7 pieces: approval requests
// (tasks 53 and 55), the resource registry (53), the cost ledger and the
// policy versions (51). factoryd's store implements them.

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

// PolicyVersion is the current policy_versions row.
type PolicyVersion struct {
	ID       int64
	SHA256   string
	LoadedAt time.Time
	Document json.RawMessage
}

// Policies reads the active policy.
type Policies interface {
	CurrentPolicy(ctx context.Context) (PolicyVersion, error)
}

// CostDay is one day's ledger total per kind (model, tool, resource,
// settlement).
type CostDay struct {
	Day    time.Time
	ByKind map[string]float64
}

// Costs reads the ledger: per day and per task over [from, to].
type Costs interface {
	CostsByDay(ctx context.Context, from, to time.Time) ([]CostDay, error)
	CostsByTask(ctx context.Context, from, to time.Time) (map[string]float64, error)
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
