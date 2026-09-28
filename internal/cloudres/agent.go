package cloudres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// HoldMargin keeps a commission's reservation past its lease, so the
// collector's final accrual books against it before factoryd's sweep could
// book it whole.
const HoldMargin = 24 * time.Hour

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

// EnvelopeViolations lists how a commission exceeds the project's
// envelope; none means it fits.
func EnvelopeViolations(env policy.Envelope, use EnvelopeUse, serverType string, leaseMinutes, volumeGB int, reservation float64) []string {
	var out []string
	if !slices.Contains(env.ServerTypes, serverType) {
		out = append(out, fmt.Sprintf("server type %s is not in the envelope's types %v", serverType, env.ServerTypes))
	}
	if leaseMinutes > env.MaxLeaseHours*60 {
		out = append(out, fmt.Sprintf("a lease of %d minutes exceeds the envelope's %d h", leaseMinutes, env.MaxLeaseHours))
	}
	if volumeGB > env.VolumeMaxGB {
		out = append(out, fmt.Sprintf("a %d GB volume exceeds the envelope's %d GB", volumeGB, env.VolumeMaxGB))
	}
	if use.OpenServers+1 > env.MaxCount {
		out = append(out, fmt.Sprintf("the project already holds %d of its %d servers", use.OpenServers, env.MaxCount))
	}
	if use.MonthEUR+reservation > env.MonthlyEUR+1e-12 {
		out = append(out, fmt.Sprintf("the reservation of %.6f EUR on top of %.6f EUR this month exceeds the envelope's %.2f EUR a month",
			reservation, use.MonthEUR, env.MonthlyEUR))
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
	c := Commission{Violations: EnvelopeViolations(env, use, r.ServerType, r.LeaseMinutes, r.VolumeGB, r.Quote.ReservationEUR)}
	if len(c.Violations) > 0 {
		var status string
		err := tx.QueryRowContext(ctx,
			`SELECT id, status, decision_note FROM approval_requests
			 WHERE kind = 'resource' AND task_id = $1 AND spec->>'server_type' = $2
			   AND (spec->>'lease_minutes')::int >= $3 AND (spec->>'volume_gb')::int >= $4
			   AND NOT EXISTS (SELECT 1 FROM cloud_resources c WHERE c.approval_id = approval_requests.id)
			 ORDER BY status = 'approved' DESC, status = 'pending' DESC, id DESC LIMIT 1`,
			r.TaskID, r.ServerType, r.LeaseMinutes, r.VolumeGB).Scan(&c.ApprovalID, &status, &c.Note)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			spec, err := json.Marshal(map[string]any{
				"server_type": r.ServerType, "lease_minutes": r.LeaseMinutes, "volume_gb": r.VolumeGB,
				"location": r.Location, "purpose": r.Purpose, "reservation_eur": r.Quote.ReservationEUR,
				"hourly_eur": r.Quote.ServerHourly + r.Quote.VolumeHourly, "beyond_envelope": c.Violations,
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
	if hold.ID, err = policy.InsertHold(ctx, tx, hold, lease.Sub(r.Now)+HoldMargin); err != nil {
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

// Accrue books every agent resource's started hours not yet booked: a
// resource ledger row (ref resource/<id>) on its task, the task's spend,
// and the same amount off its commission's reservation. A resource never
// created (no provider id) costs nothing. Then it drops the reservations
// of commissions whose rows are all closed and fully booked: the unused
// rest returns to the task, the day and the project. It returns the EUR
// booked.
func (a AgentStore) Accrue(ctx context.Context) (float64, error) {
	rows, err := a.DB.QueryContext(ctx, `SELECT id FROM cloud_resources
		WHERE project = 'agent' AND provider_id <> '' AND price_per_hour_eur > 0 AND accrued_hours < `+dueHours+` ORDER BY id`)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close() // #nosec G104 -- already returning the scan error
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	var total float64
	var errs []error
	for _, id := range ids {
		eur, err := a.accrueRow(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("accrue resource %d: %w", id, err))
		}
		total += eur
	}
	if _, err := a.DB.ExecContext(ctx, `DELETE FROM holds WHERE kind = 'resource' AND id IN (
		SELECT hold_id FROM cloud_resources WHERE hold_id IS NOT NULL GROUP BY hold_id
		HAVING bool_and(state IN ('deleted', 'vanished')
		                AND (provider_id = '' OR price_per_hour_eur = 0 OR accrued_hours >= `+dueHours+`)))`); err != nil {
		errs = append(errs, fmt.Errorf("drop settled reservations: %w", err))
	}
	return total, errors.Join(errs...)
}

func (a AgentStore) accrueRow(ctx context.Context, id int64) (float64, error) {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	var owner string
	var price float64
	var accrued, due int
	var hold int64
	if err := tx.QueryRowContext(ctx, `SELECT owner_ref, price_per_hour_eur::float8, accrued_hours, `+dueHours+`, COALESCE(hold_id, 0)
		FROM cloud_resources WHERE id = $1 FOR UPDATE`, id).Scan(&owner, &price, &accrued, &due, &hold); err != nil {
		return 0, err
	}
	if due <= accrued {
		return 0, nil
	}
	eur := float64(due-accrued) * price
	if _, err := tx.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur) VALUES ($1, 'resource', $2, $3)`,
		owner, "resource/"+strconv.FormatInt(id, 10), eur); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET spent_eur = spent_eur + $2 WHERE id = $1`, owner, eur); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE holds SET eur = GREATEST(eur - $2, 0) WHERE id = $1`, hold, eur); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cloud_resources SET accrued_hours = $2, accrued_eur = accrued_eur + $3 WHERE id = $1`,
		id, due, eur); err != nil {
		return 0, err
	}
	return eur, tx.Commit()
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

// LiveAddresses are the IPv4s of every live leased (or promoted) agent
// server by owning task: what the task's sandbox egress admits.
func (a AgentStore) LiveAddresses(ctx context.Context) (map[string][]string, error) {
	rows, err := a.DB.QueryContext(ctx, `SELECT owner_ref, address FROM cloud_resources
		WHERE project = 'agent' AND kind = 'server' AND state = 'live' AND address <> ''
		  AND (promoted OR lease_expires > now()) ORDER BY owner_ref, address`)
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
// against the project's month from then on. For the console.
func (a AgentStore) Promote(ctx context.Context, id int64) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	var hold int64
	err = tx.QueryRowContext(ctx, `SELECT hold_id FROM cloud_resources
		WHERE id = $1 AND project = 'agent' AND hold_id IS NOT NULL AND state IN `+openStates, id).Scan(&hold)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("resource %d is not an open agent resource", id)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cloud_resources SET promoted = true WHERE hold_id = $1 AND state IN `+openStates, hold); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM holds WHERE id = $1`, hold); err != nil {
		return err
	}
	return tx.Commit()
}

// EndLease ends the lease of row id's open commission now (a promoted one
// is no longer promoted): the collector deletes it on its next pass. The
// console's release, since only the gateway holds the provider token.
func (a AgentStore) EndLease(ctx context.Context, id int64) error {
	res, err := a.DB.ExecContext(ctx, `UPDATE cloud_resources SET lease_expires = now(), promoted = false
		WHERE state IN `+openStates+` AND hold_id = (SELECT hold_id FROM cloud_resources WHERE id = $1 AND project = 'agent')`, id)
	if n, _ := affected(res, err); !n && err == nil {
		return fmt.Errorf("resource %d is not an open agent resource", id)
	}
	return err
}
