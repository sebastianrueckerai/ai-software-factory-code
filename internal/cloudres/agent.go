package cloudres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/hcloud"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// AgentProject is the Hetzner project agents commission into; only the
// gateway holds its token.
const AgentProject = "agent"

// HoursPerMonth prorates a monthly price (a volume's per GB-month) to an
// hour: the calendar year's 8760 hours over 12 months.
const HoursPerMonth = 730

// AgentInterval is the agent collector's pass interval. A lease ends at
// the last pass before it runs out (LeaseCutoff before its end): the
// collector deletes it then, and factoryd's egress drops its address at
// the same cutoff (ruling 124).
const (
	AgentInterval = 2 * time.Minute
	LeaseCutoff   = AgentInterval + LeaseMargin
)

// MaxPendingRequests caps a task's pending resource approval requests.
const MaxPendingRequests = 3

// Quote prices one commission: a server with its primary IPv4 and an
// optional volume, for a lease billed per started hour.
type Quote struct {
	Hours        int     // started hours the lease covers
	ServerHourly float64 // the server type plus its primary IPv4, EUR/h
	VolumeHourly float64 // the volume's GB-month price prorated, EUR/h
	// ReservationEUR is Hours at both hourly prices: what the task's,
	// the day's and the project's month headroom hold until accrual.
	ReservationEUR float64
	// MonthlyEUR is what the resources cost for a whole month: the
	// approval card's figure.
	MonthlyEUR float64
}

// PriceCommission prices serverType with its IPv4 and volumeGB (0 = none)
// in location from the provider's catalogue for leaseMinutes.
func PriceCommission(p hcloud.Pricing, location, serverType string, leaseMinutes, volumeGB int) (Quote, error) {
	if leaseMinutes < 1 {
		return Quote{}, errors.New("the lease must be at least one minute")
	}
	server, err := p.ServerHourlyNet(serverType, location)
	if err != nil {
		return Quote{}, err
	}
	ip, err := p.PrimaryIPHourlyNet("ipv4", location)
	if err != nil {
		return Quote{}, err
	}
	serverMonth, err := p.ServerMonthlyNet(serverType, location)
	if err != nil {
		return Quote{}, err
	}
	ipMonth, err := p.PrimaryIPMonthlyNet("ipv4", location)
	if err != nil {
		return Quote{}, err
	}
	q := Quote{Hours: (leaseMinutes + 59) / 60, ServerHourly: server + ip, MonthlyEUR: serverMonth + ipMonth}
	if volumeGB > 0 {
		gbMonth, err := p.VolumeGBMonthNet()
		if err != nil {
			return Quote{}, err
		}
		q.VolumeHourly = float64(volumeGB) * gbMonth / HoursPerMonth
		q.MonthlyEUR += float64(volumeGB) * gbMonth
	}
	q.ReservationEUR = float64(q.Hours) * (q.ServerHourly + q.VolumeHourly)
	return q, nil
}

// EnvelopeUse is what a project holds now: open servers, and this UTC
// month's resource spend plus its open reservations.
type EnvelopeUse struct {
	OpenServers int
	MonthEUR    float64
}

// Violation is one way a commission exceeds the project's envelope: its
// Kind (type, lease, volume, count, month) and the text shown to the agent
// and the human.
type Violation struct {
	Kind string
	Text string
}

// EnvelopeViolations lists how a commission exceeds the project's
// envelope; none means it fits.
func EnvelopeViolations(env policy.Envelope, use EnvelopeUse, serverType string, leaseMinutes, volumeGB int, reservation float64) []Violation {
	var out []Violation
	add := func(kind, format string, a ...any) { out = append(out, Violation{kind, fmt.Sprintf(format, a...)}) }
	if !slices.Contains(env.ServerTypes, serverType) {
		add("type", "server type %s is not in the envelope's types %v", serverType, env.ServerTypes)
	}
	if leaseMinutes > env.MaxLeaseHours*60 {
		add("lease", "a lease of %d minutes exceeds the envelope's %d h", leaseMinutes, env.MaxLeaseHours)
	}
	if volumeGB > env.VolumeMaxGB {
		add("volume", "a %d GB volume exceeds the envelope's %d GB", volumeGB, env.VolumeMaxGB)
	}
	if use.OpenServers+1 > env.MaxCount {
		add("count", "the project already holds %d of its %d servers", use.OpenServers, env.MaxCount)
	}
	if use.MonthEUR+reservation > env.MonthlyEUR+1e-12 {
		add("month", "the reservation of %.6f EUR on top of %.6f EUR this month exceeds the envelope's %.2f EUR a month",
			reservation, use.MonthEUR, env.MonthlyEUR)
	}
	return out
}

// IPv4Sources normalizes firewall sources: each an IPv4 address or CIDR,
// trimmed, a bare address as /32.
func IPv4Sources(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, errors.New("no allowed sources")
	}
	out := make([]string, len(in))
	for i, src := range in {
		src = strings.TrimSpace(src)
		if !strings.Contains(src, "/") {
			src += "/32"
		}
		ip, _, err := net.ParseCIDR(src)
		if err != nil || ip.To4() == nil {
			return nil, fmt.Errorf("allowed source %q is not an IPv4 address or CIDR", in[i])
		}
		out[i] = src
	}
	return out, nil
}

// LabelValue makes s a Hetzner label value: letters, digits, '-', '_' and
// '.', at most 63 characters; a repository's '/' becomes "--".
func LabelValue(s string) string {
	s = strings.ReplaceAll(s, "/", "--")
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, s)
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}

// CommissionRequest is one commission_server call, priced.
type CommissionRequest struct {
	TaskID     string
	Repo       string // the task's repository, owner/name: the envelope's project
	Session    string
	ServerType string
	Purpose    string
	// LeaseMinutes and VolumeGB (0 = none) as the agent asked.
	LeaseMinutes int
	VolumeGB     int
	Location     string
	Quote        Quote
	Default      policy.Envelope // the policy's envelope_default
	DayEUR       float64
	Now          time.Time
}

// Commission outcomes.
const (
	Reserved = "reserved" // the hold and the rows exist; create the resources
	Pending  = "pending"  // beyond the envelope: an approval request waits
	Denied   = "denied"   // a human denied the same request
)

type Commission struct {
	Status     string
	ApprovalID int64
	Violations []string
	Note       string // the human's decision note on a denial
	Hold       policy.Hold
	// Rows are the commission's registry rows in create order: firewall,
	// server, then the volume when one was asked for.
	Rows []Resource
}

// AgentStore is the agent commissioning's Postgres state: envelopes,
// reservations, accrual, the approval requests (Task 55's table) and the
// leases factoryd admits into task egress.
type AgentStore struct{ DB *sql.DB }

const openStates = `('requested', 'live', 'deleting')`

// Envelope is repo's envelope row; false when the project has none yet.
func (a AgentStore) Envelope(ctx context.Context, repo string) (policy.Envelope, bool, error) {
	var env policy.Envelope
	var types []byte
	err := a.DB.QueryRowContext(ctx,
		`SELECT monthly_eur::float8, server_types, max_count, max_lease_hours, volume_max_gb FROM envelopes WHERE project = $1`, repo,
	).Scan(&env.MonthlyEUR, &types, &env.MaxCount, &env.MaxLeaseHours, &env.VolumeMaxGB)
	if errors.Is(err, sql.ErrNoRows) {
		return policy.Envelope{}, false, nil
	}
	if err != nil {
		return policy.Envelope{}, false, err
	}
	return env, true, json.Unmarshal(types, &env.ServerTypes)
}

// Use is what repo holds against its envelope now.
func (a AgentStore) Use(ctx context.Context, repo string) (EnvelopeUse, error) {
	return envelopeUse(ctx, a.DB, repo)
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func envelopeUse(ctx context.Context, q rowQuerier, repo string) (EnvelopeUse, error) {
	var u EnvelopeUse
	err := q.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM cloud_resources
		          WHERE project = 'agent' AND repo = $1 AND kind = 'server' AND state IN `+openStates+`),
		        (COALESCE((SELECT sum(l.eur) FROM ledger l
		          WHERE l.kind = 'resource' AND l.day >= date_trunc('month', now() AT TIME ZONE 'UTC')::date
		            AND l.ref IN (SELECT 'resource/' || id FROM cloud_resources WHERE project = 'agent' AND repo = $1)), 0)
		       + COALESCE((SELECT sum(eur) FROM holds WHERE kind = 'resource'
		            AND id IN (SELECT hold_id FROM cloud_resources WHERE project = 'agent' AND repo = $1)), 0))::float8`,
		repo).Scan(&u.OpenServers, &u.MonthEUR)
	return u, err
}

// Reserve decides a commission in one transaction under the headroom lock:
// the project's envelope (copied from the default on its first request),
// then, beyond it, an approved request of the task for the same or a
// larger spec, else the pending or denied one, else a new approval
// request; within it (or approved), the reservation against the task's
// and the day's headroom (a *policy.Refusal when it does not fit) and the
// registry rows, written before any resource is created.
func (a AgentStore) Reserve(ctx context.Context, r CommissionRequest) (Commission, error) {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return Commission{}, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	if err := policy.LockHeadroom(ctx, tx); err != nil {
		return Commission{}, err
	}
	types, err := json.Marshal(r.Default.ServerTypes)
	if err != nil {
		return Commission{}, err
	}
	d := r.Default
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO envelopes (project, monthly_eur, server_types, max_count, max_lease_hours, volume_max_gb, set_by)
		 VALUES ($1, $2, $3::jsonb, $4, $5, $6, 'default') ON CONFLICT (project) DO NOTHING`,
		r.Repo, d.MonthlyEUR, types, d.MaxCount, d.MaxLeaseHours, d.VolumeMaxGB); err != nil {
		return Commission{}, err
	}
	var env policy.Envelope
	var envTypes []byte
	if err := tx.QueryRowContext(ctx,
		`SELECT monthly_eur::float8, server_types, max_count, max_lease_hours, volume_max_gb FROM envelopes WHERE project = $1`, r.Repo,
	).Scan(&env.MonthlyEUR, &envTypes, &env.MaxCount, &env.MaxLeaseHours, &env.VolumeMaxGB); err != nil {
		return Commission{}, err
	}
	if err := json.Unmarshal(envTypes, &env.ServerTypes); err != nil {
		return Commission{}, err
	}
	use, err := envelopeUse(ctx, tx, r.Repo)
	if err != nil {
		return Commission{}, err
	}
	violations := EnvelopeViolations(env, use, r.ServerType, r.LeaseMinutes, r.VolumeGB, r.Quote.ReservationEUR)
	c := Commission{}
	var kinds []string
	for _, v := range violations {
		c.Violations = append(c.Violations, v.Text)
		kinds = append(kinds, v.Kind)
	}
	if len(violations) > 0 {
		// a request covers this one when it is for the same type, at least
		// this lease and volume, and names every way this one is beyond
		kindsJSON, err := json.Marshal(kinds)
		if err != nil {
			return Commission{}, err
		}
		var status string
		err = tx.QueryRowContext(ctx,
			`SELECT id, status, decision_note FROM approval_requests
			 WHERE kind = 'resource' AND task_id = $1 AND spec->>'server_type' = $2
			   AND (spec->>'lease_minutes')::int >= $3 AND (spec->>'volume_gb')::int >= $4
			   AND COALESCE(spec->'beyond_kinds', '[]'::jsonb) @> $5::jsonb
			   AND NOT EXISTS (SELECT 1 FROM cloud_resources c WHERE c.approval_id = approval_requests.id)
			 ORDER BY status = 'approved' DESC, status = 'pending' DESC, id DESC LIMIT 1`,
			r.TaskID, r.ServerType, r.LeaseMinutes, r.VolumeGB, kindsJSON).Scan(&c.ApprovalID, &status, &c.Note)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			var pending int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM approval_requests
				WHERE kind = 'resource' AND task_id = $1 AND status = 'pending'`, r.TaskID).Scan(&pending); err != nil {
				return Commission{}, err
			}
			if pending >= MaxPendingRequests {
				return Commission{}, &policy.Refusal{Decision: "refused_approvals", Reason: fmt.Sprintf(
					"the task already has %d pending resource requests (at most %d); wait for a decision (resource_request_status) or work within the envelope",
					pending, MaxPendingRequests)}
			}
			spec, err := json.Marshal(map[string]any{
				"server_type": r.ServerType, "lease_minutes": r.LeaseMinutes, "volume_gb": r.VolumeGB,
				"location": r.Location, "purpose": r.Purpose, "reservation_eur": r.Quote.ReservationEUR,
				"hourly_eur": r.Quote.ServerHourly + r.Quote.VolumeHourly, "beyond_envelope": c.Violations,
				"beyond_kinds": kinds,
			})
			if err != nil {
				return Commission{}, err
			}
			if err := tx.QueryRowContext(ctx,
				`INSERT INTO approval_requests (kind, task_id, project, spec, reason, monthly_eur)
				 VALUES ('resource', $1, $2, $3::jsonb, $4, $5) RETURNING id`,
				r.TaskID, r.Repo, spec, r.Purpose+" (beyond the envelope: "+strings.Join(c.Violations, "; ")+")",
				r.Quote.MonthlyEUR).Scan(&c.ApprovalID); err != nil {
				return Commission{}, err
			}
			c.Status = Pending
			return c, tx.Commit()
		case err != nil:
			return Commission{}, err
		case status == "pending":
			c.Status = Pending
			return c, tx.Commit()
		case status == "denied":
			c.Status = Denied
			return c, tx.Commit()
		}
	}

	free, err := policy.FreeHeadroom(ctx, tx, r.TaskID, r.DayEUR)
	if err != nil {
		return Commission{}, err
	}
	hold, err := policy.FitResource(free.Task, free.Day, policy.HoldRequest{
		TaskID: r.TaskID, Kind: policy.KindResource, Ref: r.ServerType, EUR: r.Quote.ReservationEUR})
	if err != nil {
		return Commission{}, err
	}
	lease := r.Now.Add(time.Duration(r.LeaseMinutes) * time.Minute)
	// accrual books and drops the reservation; factoryd's sweep skips
	// resource holds, so their expiry is informational
	if hold.ID, err = policy.InsertHold(ctx, tx, hold, lease.Sub(r.Now)); err != nil {
		return Commission{}, err
	}
	c.Hold = hold
	base := Resource{
		Project: AgentProject, OwnerRef: r.TaskID, LeaseExpires: lease, Repo: r.Repo, SessionRef: r.Session,
		Purpose: r.Purpose, HoldID: hold.ID, ApprovalID: c.ApprovalID,
		Labels: map[string]string{"task": LabelValue(r.TaskID), "project": LabelValue(r.Repo)},
	}
	add := func(kind, size string, price float64) error {
		row := base
		row.Kind, row.Size, row.PricePerHourEUR = kind, size, price
		row.Name = fmt.Sprintf("agent-%s-%d-%s", LabelValue(r.TaskID), hold.ID, kind)
		row.Labels = map[string]string{"task": base.Labels["task"], "project": base.Labels["project"]}
		out, err := insert(ctx, tx, row)
		c.Rows = append(c.Rows, out)
		return err
	}
	if err := add(KindFirewall, "", 0); err != nil {
		return Commission{}, err
	}
	if err := add(KindServer, r.ServerType, r.Quote.ServerHourly); err != nil {
		return Commission{}, err
	}
	if r.VolumeGB > 0 {
		if err := add(KindVolume, strconv.Itoa(r.VolumeGB)+"GB", r.Quote.VolumeHourly); err != nil {
			return Commission{}, err
		}
	}
	c.Status = Reserved
	return c, tx.Commit()
}

// Session is the task's latest session id, "" before its first.
func (a AgentStore) Session(ctx context.Context, taskID string) (string, error) {
	var id string
	err := a.DB.QueryRowContext(ctx, `SELECT id FROM sessions WHERE task_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, taskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// SetAddress records a server's public IPv4.
func (a AgentStore) SetAddress(ctx context.Context, id int64, address string) error {
	_, err := a.DB.ExecContext(ctx, `UPDATE cloud_resources SET address = $2 WHERE id = $1`, id, address)
	return err
}

// dueHours is a row's billed hours: every started hour from its creation
// to its deletion (now while it lives), at least one.
const dueHours = `GREATEST(1, ceil(extract(epoch FROM (COALESCE(deleted_at, now()) - created_at)) / 3600))::int`

// Accrual is what one accrual pass booked, and the bookings that went past
// their commission's reservation.
type Accrual struct {
	EUR      float64
	Overruns []Overrun
}

// Overrun is an accrual booked in full beyond what its reservation still
// held: the resource lived longer than its lease paid for (ruling 124).
type Overrun struct {
	TaskID     string
	ResourceID int64
	EUR        float64 // booked
	HeldEUR    float64 // what the reservation still held
}

// Accrue books every agent resource's started hours not yet booked: a
// resource ledger row (ref resource/<id>) on its task, the task's spend,
// and the same amount off its commission's reservation, under the
// headroom lock. A booking beyond what the reservation still holds is
// booked in full and returned as an overrun (a promoted resource has no
// reservation and never overruns). A resource never created (no provider
// id) costs nothing. Then it drops the reservations of commissions whose
// rows are all closed and fully booked: the unused rest returns to the
// task, the day and the project.
func (a AgentStore) Accrue(ctx context.Context) (Accrual, error) {
	var out Accrual
	rows, err := a.DB.QueryContext(ctx, `SELECT id FROM cloud_resources
		WHERE project = 'agent' AND provider_id <> '' AND price_per_hour_eur > 0 AND accrued_hours < `+dueHours+` ORDER BY id`)
	if err != nil {
		return out, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close() // #nosec G104 -- already returning the scan error
			return out, err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return out, err
	}
	var errs []error
	for _, id := range ids {
		eur, over, err := a.accrueRow(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("accrue resource %d: %w", id, err))
		}
		out.EUR += eur
		if over != nil {
			out.Overruns = append(out.Overruns, *over)
		}
	}
	if _, err := a.DB.ExecContext(ctx, `DELETE FROM holds WHERE kind = 'resource' AND id IN (
		SELECT hold_id FROM cloud_resources WHERE hold_id IS NOT NULL GROUP BY hold_id
		HAVING bool_and(state IN ('deleted', 'vanished')
		                AND (provider_id = '' OR price_per_hour_eur = 0 OR accrued_hours >= `+dueHours+`)))`); err != nil {
		errs = append(errs, fmt.Errorf("drop settled reservations: %w", err))
	}
	return out, errors.Join(errs...)
}

func (a AgentStore) accrueRow(ctx context.Context, id int64) (float64, *Overrun, error) {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	if err := policy.LockHeadroom(ctx, tx); err != nil {
		return 0, nil, err
	}
	var owner string
	var price float64
	var accrued, due int
	var hold int64
	var promoted bool
	if err := tx.QueryRowContext(ctx, `SELECT owner_ref, price_per_hour_eur::float8, accrued_hours, `+dueHours+`, COALESCE(hold_id, 0), promoted
		FROM cloud_resources WHERE id = $1 FOR UPDATE`, id).Scan(&owner, &price, &accrued, &due, &hold, &promoted); err != nil {
		return 0, nil, err
	}
	if due <= accrued {
		return 0, nil, nil
	}
	eur := float64(due-accrued) * price
	var held float64
	err = tx.QueryRowContext(ctx, `SELECT eur::float8 FROM holds WHERE id = $1`, hold).Scan(&held)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, nil, err
	}
	var over *Overrun
	if !promoted && eur > held+1e-12 {
		over = &Overrun{TaskID: owner, ResourceID: id, EUR: eur, HeldEUR: held}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur) VALUES ($1, 'resource', $2, $3)`,
		owner, "resource/"+strconv.FormatInt(id, 10), eur); err != nil {
		return 0, nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET spent_eur = spent_eur + $2 WHERE id = $1`, owner, eur); err != nil {
		return 0, nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE holds SET eur = GREATEST(eur - $2, 0) WHERE id = $1`, hold, eur); err != nil {
		return 0, nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cloud_resources SET accrued_hours = $2, accrued_eur = accrued_eur + $3 WHERE id = $1`,
		id, due, eur); err != nil {
		return 0, nil, err
	}
	return eur, over, tx.Commit()
}

// Owned lists every agent row of a task, oldest first.
func (a AgentStore) Owned(ctx context.Context, taskID string) ([]Resource, error) {
	return PG(a).query(ctx, `SELECT `+resourceColumns+` FROM cloud_resources
		WHERE project = 'agent' AND owner_ref = $1 ORDER BY id`, taskID)
}

// Group lists the open rows of one commission (its reservation id).
func (a AgentStore) Group(ctx context.Context, holdID int64) ([]Resource, error) {
	return PG(a).query(ctx, `SELECT `+resourceColumns+` FROM cloud_resources
		WHERE project = 'agent' AND hold_id = $1 AND state IN `+openStates+` ORDER BY id`, holdID)
}

// LiveAddresses are the IPv4s of every live agent server by owning task
// whose lease runs past the cutoff (or that is promoted): what the task's
// sandbox egress admits. The cutoff is the collector's, so the address
// leaves when the collector deletes the server.
func (a AgentStore) LiveAddresses(ctx context.Context) (map[string][]string, error) {
	rows, err := a.DB.QueryContext(ctx, `SELECT owner_ref, address FROM cloud_resources
		WHERE project = 'agent' AND kind = 'server' AND state = 'live' AND address <> ''
		  AND (promoted OR lease_expires > now() + make_interval(secs => $1)) ORDER BY owner_ref, address`, LeaseCutoff.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var owner, addr string
		if err := rows.Scan(&owner, &addr); err != nil {
			return nil, err
		}
		out[owner] = append(out[owner], addr)
	}
	return out, rows.Err()
}

// Approval is a resource approval request as its task sees it.
type Approval struct {
	ID     int64
	Status string // pending | approved | denied
	Note   string
	Spec   map[string]any
}

// Approval reads a task's own resource approval request; false when the
// task has none by that id.
func (a AgentStore) Approval(ctx context.Context, id int64, taskID string) (Approval, bool, error) {
	ap := Approval{ID: id}
	var spec []byte
	err := a.DB.QueryRowContext(ctx, `SELECT status, decision_note, spec FROM approval_requests
		WHERE id = $1 AND kind = 'resource' AND task_id = $2`, id, taskID).Scan(&ap.Status, &ap.Note, &spec)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, false, nil
	}
	if err != nil {
		return Approval{}, false, err
	}
	return ap, true, json.Unmarshal(spec, &ap.Spec)
}

// Promote makes the open commission of row id project-owned: no lease, no
// end with its task, its reservation dropped; its accruing cost counts
// against the project's month from then on. A released commission, or one
// whose lease has ended, is refused (ErrLeaseEnded): the collector is
// about to delete it. For the console; by is the human.
func (a AgentStore) Promote(ctx context.Context, id int64, by string) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	var hold int64
	var ended bool
	err = tx.QueryRowContext(ctx, `SELECT hold_id, (released_by <> '' OR (NOT promoted AND lease_expires <= now())) FROM cloud_resources
		WHERE id = $1 AND project = 'agent' AND hold_id IS NOT NULL AND state IN `+openStates+` FOR UPDATE`, id).Scan(&hold, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("resource %d: %w", id, ErrNotOpen)
	}
	if err != nil {
		return err
	}
	if ended {
		return fmt.Errorf("resource %d: %w", id, ErrLeaseEnded)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cloud_resources SET promoted = true, promoted_by = $2 WHERE hold_id = $1 AND state IN `+openStates, hold, by); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM holds WHERE id = $1`, hold); err != nil {
		return err
	}
	return tx.Commit()
}

// EndLease ends the lease of row id's open commission now (a promoted one
// is no longer promoted): the collector deletes it on its next pass. The
// console's release, since only the gateway holds the provider token; by
// is the human.
func (a AgentStore) EndLease(ctx context.Context, id int64, by string) error {
	res, err := a.DB.ExecContext(ctx, `UPDATE cloud_resources SET lease_expires = now(), promoted = false, released_by = $2
		WHERE state IN `+openStates+` AND hold_id = (SELECT hold_id FROM cloud_resources WHERE id = $1 AND project = 'agent')`, id, by)
	if n, _ := affected(res, err); !n && err == nil {
		return fmt.Errorf("resource %d: %w", id, ErrNotOpen)
	}
	return err
}

// Promote's and EndLease's refusals, for the console's messages.
var (
	ErrNotOpen    = errors.New("not an open agent resource")
	ErrLeaseEnded = errors.New("its lease has ended or it was released; the collector deletes it")
)

// Commissions lists the agent rows open now or closed after since, by
// project, task and commission: the console's resources page.
func (a AgentStore) Commissions(ctx context.Context, since time.Time) ([]Resource, error) {
	return PG(a).query(ctx, `SELECT `+resourceColumns+` FROM cloud_resources
		WHERE project = 'agent' AND (state IN `+openStates+` OR deleted_at > $1)
		ORDER BY repo, owner_ref, COALESCE(hold_id, 0), id`, since)
}

// EnvelopeRecord is a project's envelope row, who set it last, and what
// the project holds against it now.
type EnvelopeRecord struct {
	Project string
	policy.Envelope
	SetBy string
	SetAt time.Time
	Use   EnvelopeUse
}

// Envelopes lists every project's envelope with its use.
func (a AgentStore) Envelopes(ctx context.Context) ([]EnvelopeRecord, error) {
	rows, err := a.DB.QueryContext(ctx, `SELECT project, monthly_eur::float8, server_types, max_count, max_lease_hours,
		volume_max_gb, set_by, set_at FROM envelopes ORDER BY project`)
	if err != nil {
		return nil, err
	}
	var out []EnvelopeRecord
	for rows.Next() {
		var e EnvelopeRecord
		var types []byte
		if err := rows.Scan(&e.Project, &e.MonthlyEUR, &types, &e.MaxCount, &e.MaxLeaseHours, &e.VolumeMaxGB, &e.SetBy, &e.SetAt); err != nil {
			rows.Close() // #nosec G104 -- already returning the scan error
			return nil, err
		}
		if err := json.Unmarshal(types, &e.ServerTypes); err != nil {
			rows.Close() // #nosec G104 -- already returning the decode error
			return nil, err
		}
		out = append(out, e)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Use, err = envelopeUse(ctx, a.DB, out[i].Project); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetEnvelope writes project's envelope as by (the console's edit),
// creating the row when the project has none yet.
func (a AgentStore) SetEnvelope(ctx context.Context, project string, env policy.Envelope, by string) error {
	types, err := json.Marshal(env.ServerTypes)
	if err != nil {
		return err
	}
	_, err = a.DB.ExecContext(ctx,
		`INSERT INTO envelopes (project, monthly_eur, server_types, max_count, max_lease_hours, volume_max_gb, set_by)
		 VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7)
		 ON CONFLICT (project) DO UPDATE SET monthly_eur = $2, server_types = $3::jsonb, max_count = $4,
		   max_lease_hours = $5, volume_max_gb = $6, set_by = $7, set_at = now()`,
		project, env.MonthlyEUR, types, env.MaxCount, env.MaxLeaseHours, env.VolumeMaxGB, by)
	return err
}

// requestSpec is what a resource approval request asks for (Reserve's
// spec).
type requestSpec struct {
	ServerType     string  `json:"server_type"`
	LeaseMinutes   int     `json:"lease_minutes"`
	VolumeGB       int     `json:"volume_gb"`
	ReservationEUR float64 `json:"reservation_eur"`
}

// Widen is env grown just enough that the request fits beside use: its
// server type, its lease, its volume, one more server, and its
// reservation on top of the month (rounded up to the cent).
func Widen(env policy.Envelope, use EnvelopeUse, serverType string, leaseMinutes, volumeGB int, reservation float64) policy.Envelope {
	out := env
	out.ServerTypes = slices.Clone(env.ServerTypes)
	if !slices.Contains(out.ServerTypes, serverType) {
		out.ServerTypes = append(out.ServerTypes, serverType)
	}
	out.MaxLeaseHours = max(env.MaxLeaseHours, (leaseMinutes+59)/60)
	out.VolumeMaxGB = max(env.VolumeMaxGB, volumeGB)
	out.MaxCount = max(env.MaxCount, use.OpenServers+1)
	out.MonthlyEUR = max(env.MonthlyEUR, math.Ceil((use.MonthEUR+reservation)*100-1e-9)/100)
	return out
}

// ApproveForProject is a human's project-wide approve of resource request
// id: under the headroom lock, the project's envelope widens to fit the
// request (Widen) and the request is approved, both as by. It returns the
// widened envelope.
func (a AgentStore) ApproveForProject(ctx context.Context, id int64, by string) (policy.Envelope, error) {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return policy.Envelope{}, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	if err := policy.LockHeadroom(ctx, tx); err != nil {
		return policy.Envelope{}, err
	}
	var project string
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT project, spec FROM approval_requests
		WHERE id = $1 AND kind = 'resource' AND status = 'pending' FOR UPDATE`, id).Scan(&project, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return policy.Envelope{}, fmt.Errorf("no pending resource request %d", id)
	}
	if err != nil {
		return policy.Envelope{}, err
	}
	var spec requestSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return policy.Envelope{}, fmt.Errorf("resource request %d: %w", id, err)
	}
	var env policy.Envelope
	var types []byte
	err = tx.QueryRowContext(ctx, `SELECT monthly_eur::float8, server_types, max_count, max_lease_hours, volume_max_gb
		FROM envelopes WHERE project = $1`, project).Scan(&env.MonthlyEUR, &types, &env.MaxCount, &env.MaxLeaseHours, &env.VolumeMaxGB)
	if err != nil {
		return policy.Envelope{}, fmt.Errorf("envelope of %s: %w", project, err)
	}
	if err := json.Unmarshal(types, &env.ServerTypes); err != nil {
		return policy.Envelope{}, err
	}
	use, err := envelopeUse(ctx, tx, project)
	if err != nil {
		return policy.Envelope{}, err
	}
	wide := Widen(env, use, spec.ServerType, spec.LeaseMinutes, spec.VolumeGB, spec.ReservationEUR)
	if types, err = json.Marshal(wide.ServerTypes); err != nil {
		return policy.Envelope{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE envelopes SET monthly_eur = $2, server_types = $3::jsonb, max_count = $4,
		max_lease_hours = $5, volume_max_gb = $6, set_by = $7, set_at = now() WHERE project = $1`,
		project, wide.MonthlyEUR, types, wide.MaxCount, wide.MaxLeaseHours, wide.VolumeMaxGB, by); err != nil {
		return policy.Envelope{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE approval_requests SET status = 'approved', decided_by = $2, decided_at = now(),
		decision_note = 'approved for the project: its envelope was widened to fit' WHERE id = $1`, id, by); err != nil {
		return policy.Envelope{}, err
	}
	return wide, tx.Commit()
}
