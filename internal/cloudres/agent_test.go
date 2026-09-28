package cloudres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/hcloud"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema"
)

// livePricing is GET /v1/pricing for fsn1 as the agent project read it on
// 2026-09-28 (net EUR).
func livePricing(t *testing.T) hcloud.Pricing {
	t.Helper()
	var p hcloud.Pricing
	if err := json.Unmarshal([]byte(`{"currency":"EUR","volume":{"price_per_gb_month":{"net":"0.0572000000"}},
		"primary_ips":[{"type":"ipv4","prices":[{"location":"fsn1","price_hourly":{"net":"0.0008000000"},"price_monthly":{"net":"0.5000000000"}}]}],
		"server_types":[
		  {"name":"cx23","prices":[{"location":"fsn1","price_hourly":{"net":"0.0088000000"},"price_monthly":{"net":"5.4900000000"}}]},
		  {"name":"cx33","prices":[{"location":"fsn1","price_hourly":{"net":"0.0136000000"},"price_monthly":{"net":"8.4900000000"}}]}]}`), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestPriceCommission(t *testing.T) {
	p := livePricing(t)
	for _, tc := range []struct {
		name                 string
		typ                  string
		minutes, gb          int
		hours                int
		hourly, volume       float64
		reservation, monthly float64
	}{
		// one started hour: 0.0088 + 0.0008
		{"one hour", "cx23", 60, 0, 1, 0.0096, 0, 0.0096, 5.99},
		{"a minute is a started hour", "cx23", 1, 0, 1, 0.0096, 0, 0.0096, 5.99},
		{"61 minutes are two", "cx23", 61, 0, 2, 0.0096, 0, 0.0192, 5.99},
		// 4 h of cx33 + IPv4: 4 x 0.0144
		{"cx33 four hours", "cx33", 240, 0, 4, 0.0144, 0, 0.0576, 8.99},
		// 20 GB x 0.0572 = 1.144 EUR/month, / 730 h = 0.00156712328767/h;
		// 2 h x (0.0096 + 0.00156712328767) = 0.02233424657534
		{"with a volume", "cx23", 90, 20, 2, 0.0096, 1.144 / 730, 0.0192 + 2*1.144/730, 5.99 + 1.144},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := PriceCommission(p, "fsn1", tc.typ, tc.minutes, tc.gb)
			if err != nil {
				t.Fatal(err)
			}
			if q.Hours != tc.hours || !near(q.ServerHourly, tc.hourly) || !near(q.VolumeHourly, tc.volume) ||
				!near(q.ReservationEUR, tc.reservation) || !near(q.MonthlyEUR, tc.monthly) {
				t.Fatalf("quote = %+v", q)
			}
		})
	}
	if q, _ := PriceCommission(p, "fsn1", "cx23", 90, 20); math.Abs(q.ReservationEUR-0.02233424657534) > 1e-13 {
		t.Fatalf("reservation = %.14f", q.ReservationEUR)
	}
	if _, err := PriceCommission(p, "fsn1", "cx99", 60, 0); err == nil {
		t.Fatal("want an error for an unpriced type")
	}
	if _, err := PriceCommission(p, "fsn1", "cx23", 0, 0); err == nil {
		t.Fatal("want an error for a zero lease")
	}
}

var testEnvelope = policy.Envelope{MonthlyEUR: 10, ServerTypes: []string{"cx23", "cx33"}, MaxCount: 2, MaxLeaseHours: 4, VolumeMaxGB: 20,
	Standing: policy.Standing{MonthlyEUR: 20, ServerTypes: []string{"cx23", "cx33"}, MaxCount: 2}}

func TestEnvelopeViolations(t *testing.T) {
	if v := EnvelopeViolations(testEnvelope, EnvelopeUse{OpenServers: 1, MonthEUR: 9.99}, "cx23", 240, 20, 0.01); len(v) != 0 {
		t.Fatalf("at every limit: %v", v)
	}
	for name, tc := range map[string]struct {
		use     EnvelopeUse
		typ     string
		minutes int
		gb      int
		res     float64
		want    string
	}{
		"type":   {EnvelopeUse{}, "cx53", 60, 0, 0.01, "server type cx53 is not in the envelope's types [cx23 cx33]"},
		"lease":  {EnvelopeUse{}, "cx23", 241, 0, 0.01, "a lease of 241 minutes exceeds the envelope's 4 h"},
		"volume": {EnvelopeUse{}, "cx23", 60, 21, 0.01, "a 21 GB volume exceeds the envelope's 20 GB"},
		"count":  {EnvelopeUse{OpenServers: 2}, "cx23", 60, 0, 0.01, "the project already holds 2 of its 2 servers"},
		"month": {EnvelopeUse{MonthEUR: 9.995}, "cx23", 60, 0, 0.0096,
			"the reservation of 0.009600 EUR on top of 9.995000 EUR this month exceeds the envelope's 10.00 EUR a month"},
	} {
		if v := EnvelopeViolations(testEnvelope, tc.use, tc.typ, tc.minutes, tc.gb, tc.res); len(v) != 1 || v[0].Text != tc.want || v[0].Kind != name {
			t.Errorf("%s: %v, want [%s]", name, v, tc.want)
		}
	}
}

func TestLabelValue(t *testing.T) {
	if got := LabelValue("sf/ai-software-factory-code"); got != "sf--ai-software-factory-code" {
		t.Fatalf("repo label = %q", got)
	}
	if got := LabelValue("a b:" + strings.Repeat("x", 80)); len(got) != 63 || !strings.HasPrefix(got, "a_b_x") {
		t.Fatalf("label = %q", got)
	}
}

// agentDB opens a disposable Postgres (FACTORYD_TEST_DATABASE_URL) with
// the factory schema applied, wiped.
func agentDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := schematest.DSN(t, "cloudres")
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := schema.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	// the ledger is append-only by trigger; a test database is wiped whole
	if _, err := db.ExecContext(ctx, `TRUNCATE tasks, ledger, holds, cloud_resources, envelopes, approval_requests CASCADE`); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedTask(t *testing.T, db *sql.DB, id string, budget float64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository) VALUES ($1, $2, 0, 'running', 'sf/app')`, id, budget); err != nil {
		t.Fatal(err)
	}
}

func commissionReq(t *testing.T, task, typ string, minutes, gb int) CommissionRequest {
	t.Helper()
	q, err := PriceCommission(livePricing(t), "fsn1", typ, minutes, gb)
	if err != nil {
		t.Fatal(err)
	}
	return CommissionRequest{TaskID: task, Repo: "sf/app", Session: "s-1", ServerType: typ, Purpose: "load test",
		LeaseMinutes: minutes, VolumeGB: gb, Location: "fsn1", Quote: q, Default: testEnvelope, DayEUR: 20, Now: time.Now()}
}

func scalar[T any](t *testing.T, db *sql.DB, q string, args ...any) T {
	t.Helper()
	var v T
	if err := db.QueryRowContext(context.Background(), q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func TestReserveWithinEnvelope(t *testing.T) {
	db := agentDB(t)
	ctx := context.Background()
	seedTask(t, db, "t1", 1)
	a := AgentStore{DB: db}
	c, err := a.Reserve(ctx, commissionReq(t, "t1", "cx23", 90, 20))
	if err != nil || c.Status != Reserved {
		t.Fatalf("reserve = %+v, %v", c, err)
	}
	// the default was copied into the project's row
	env, ok, err := a.Envelope(ctx, "sf/app")
	if err != nil || !ok || !reflect.DeepEqual(env, testEnvelope) {
		t.Fatalf("envelope = %+v, %v, %v", env, ok, err)
	}
	if by := scalar[string](t, db, `SELECT set_by FROM envelopes WHERE project = 'sf/app'`); by != "default" {
		t.Fatalf("set_by = %s", by)
	}
	// the reservation: 2 h x (0.0096 + 20 x 0.0572 / 730)
	if held := scalar[float64](t, db, `SELECT eur::float8 FROM holds WHERE id = $1 AND kind = 'resource' AND task_id = 't1'`, c.Hold.ID); !near(held, 0.0192+2*1.144/730) {
		t.Fatalf("held = %v", held)
	}
	// the hold's expiry is the lease's (informational: the sweep skips it)
	if d := scalar[float64](t, db, `SELECT extract(epoch FROM expires_at - now())::float8 FROM holds WHERE id = $1`, c.Hold.ID); d < (90*time.Minute).Seconds()-30 || d > (90*time.Minute).Seconds()+30 {
		t.Fatalf("hold expires in %v s", d)
	}
	kinds := []string{}
	for _, r := range c.Rows {
		kinds = append(kinds, r.Kind+":"+r.Size)
		if r.State != StateRequested || r.HoldID != c.Hold.ID || r.OwnerRef != "t1" || r.Repo != "sf/app" || r.SessionRef != "s-1" ||
			r.Labels["task"] != "t1" || r.Labels["project"] != "sf--app" || r.Labels["factory"] != "agent" || r.Labels["resource"] == "" {
			t.Fatalf("row = %+v", r)
		}
	}
	if strings.Join(kinds, ",") != "firewall:,server:cx23,volume:20GB" {
		t.Fatalf("rows = %v", kinds)
	}
	if !near(c.Rows[1].PricePerHourEUR, 0.0096) || !near(c.Rows[2].PricePerHourEUR, 1.144/730) {
		t.Fatalf("prices = %v %v", c.Rows[1].PricePerHourEUR, c.Rows[2].PricePerHourEUR)
	}
	// each row's share of the reservation and its catalogue month, as stored:
	// server 2 x 0.0096 and 5.49 + 0.50, volume 2 x 1.144/730 and 20 x 0.0572
	stored, _ := a.Owned(ctx, "t1")
	if len(stored) != 3 || stored[0].ReservedEUR != 0 || stored[0].MonthlyEUR != 0 ||
		!near(stored[1].ReservedEUR, 0.0192) || !near(stored[1].MonthlyEUR, 5.99) ||
		!near(stored[2].ReservedEUR, 2*1.144/730) || !near(stored[2].MonthlyEUR, 1.144) {
		t.Fatalf("stored rows = %+v", stored)
	}
	use, err := a.Use(ctx, "sf/app")
	if err != nil || use.OpenServers != 1 || !near(use.MonthEUR, 0.0192+2*1.144/730) {
		t.Fatalf("use = %+v, %v", use, err)
	}

	// a task without the headroom is refused, and nothing is written
	seedTask(t, db, "poor", 0.005)
	_, err = a.Reserve(ctx, commissionReq(t, "poor", "cx23", 60, 0))
	var ref *policy.Refusal
	if !errors.As(err, &ref) || ref.Decision != "refused_budget" {
		t.Fatalf("poor task err = %v", err)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM cloud_resources WHERE owner_ref = 'poor'`); n != 0 {
		t.Fatalf("rows for a refused commission: %d", n)
	}
}

func TestReserveBeyondEnvelopeAsksAHuman(t *testing.T) {
	db := agentDB(t)
	ctx := context.Background()
	seedTask(t, db, "t1", 1)
	a := AgentStore{DB: db}
	// cx33 beyond the lease: 300 minutes > 4 h
	req := commissionReq(t, "t1", "cx33", 300, 0)
	c, err := a.Reserve(ctx, req)
	if err != nil || c.Status != Pending || c.ApprovalID == 0 || len(c.Violations) != 1 {
		t.Fatalf("beyond = %+v, %v", c, err)
	}
	var kind, project, reason, spec string
	var monthly float64
	if err := db.QueryRowContext(ctx, `SELECT kind, project, reason, spec::text, monthly_eur::float8 FROM approval_requests WHERE id = $1`, c.ApprovalID).
		Scan(&kind, &project, &reason, &spec, &monthly); err != nil {
		t.Fatal(err)
	}
	if kind != "resource" || project != "sf/app" || !strings.Contains(reason, "load test") || !strings.Contains(reason, "300 minutes") ||
		!strings.Contains(spec, `"server_type": "cx33"`) || !strings.Contains(spec, `"lease_minutes": 300`) || !near(monthly, 8.99) {
		t.Fatalf("request = %s %s %q %s %v", kind, project, reason, spec, monthly)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM holds`); n != 0 {
		t.Fatalf("holds = %d", n)
	}
	// asking again finds the same pending request
	again, err := a.Reserve(ctx, req)
	if err != nil || again.Status != Pending || again.ApprovalID != c.ApprovalID {
		t.Fatalf("again = %+v, %v", again, err)
	}
	ap, ok, err := a.Approval(ctx, c.ApprovalID, "t1")
	if err != nil || !ok || ap.Status != "pending" || ap.Spec.ServerType != "cx33" {
		t.Fatalf("approval = %+v, %v, %v", ap, ok, err)
	}
	if _, ok, _ := a.Approval(ctx, c.ApprovalID, "other"); ok {
		t.Fatal("another task reads the request")
	}

	// denied: the retry says so and asks nobody again
	db.ExecContext(ctx, `UPDATE approval_requests SET status = 'denied', decision_note = 'too long' WHERE id = $1`, c.ApprovalID)
	d, err := a.Reserve(ctx, req)
	if err != nil || d.Status != Denied || d.Note != "too long" || d.ApprovalID != c.ApprovalID {
		t.Fatalf("denied = %+v, %v", d, err)
	}

	// approved: the retry of the same (or a smaller) spec is reserved once
	db.ExecContext(ctx, `UPDATE approval_requests SET status = 'approved' WHERE id = $1`, c.ApprovalID)
	smaller := commissionReq(t, "t1", "cx33", 280, 0)
	ok1, err := a.Reserve(ctx, smaller)
	if err != nil || ok1.Status != Reserved || ok1.ApprovalID != c.ApprovalID || ok1.Rows[1].ApprovalID != c.ApprovalID {
		t.Fatalf("approved = %+v, %v", ok1, err)
	}
	// consumed: the next one beyond the envelope is a new request
	next, err := a.Reserve(ctx, req)
	if err != nil || next.Status != Pending || next.ApprovalID == c.ApprovalID {
		t.Fatalf("after consumption = %+v, %v", next, err)
	}

	// the count: with two open servers the third is beyond
	seedTask(t, db, "t2", 1)
	if c, err := a.Reserve(ctx, commissionReq(t, "t2", "cx23", 60, 0)); err != nil || c.Status != Reserved {
		t.Fatalf("second server = %+v, %v", c, err)
	}
	third, err := a.Reserve(ctx, commissionReq(t, "t2", "cx23", 60, 0))
	if err != nil || third.Status != Pending || !strings.Contains(strings.Join(third.Violations, ";"), "already holds 2 of its 2") {
		t.Fatalf("third = %+v, %v", third, err)
	}
}

func TestReserveMonthCap(t *testing.T) {
	db := agentDB(t)
	ctx := context.Background()
	seedTask(t, db, "t1", 20)
	a := AgentStore{DB: db}
	if _, err := db.ExecContext(ctx, `INSERT INTO envelopes (project, monthly_eur, server_types, max_count, max_lease_hours, volume_max_gb, set_by)
		VALUES ('sf/app', 0.03, '["cx23"]', 5, 4, 0, 'sebastian')`); err != nil {
		t.Fatal(err)
	}
	// 0.0192 held; a second 0.0192 would make 0.0384 > 0.03
	if c, err := a.Reserve(ctx, commissionReq(t, "t1", "cx23", 120, 0)); err != nil || c.Status != Reserved {
		t.Fatalf("first = %+v, %v", c, err)
	}
	c, err := a.Reserve(ctx, commissionReq(t, "t1", "cx23", 120, 0))
	if err != nil || c.Status != Pending || !strings.Contains(c.Violations[0], "exceeds the envelope's 0.03 EUR a month") {
		t.Fatalf("second = %+v, %v", c, err)
	}
	// the edited row is kept, not overwritten by the default
	if env, _, _ := a.Envelope(ctx, "sf/app"); env.MonthlyEUR != 0.03 {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestAccrueBooksStartedHoursAndReturnsTheRest(t *testing.T) {
	db := agentDB(t)
	ctx := context.Background()
	seedTask(t, db, "t1", 1)
	a := AgentStore{DB: db}
	reg := PG{DB: db}
	// 3 h lease of cx23 with 20 GB: reservation 3 x (0.0096 + 1.144/730)
	c, err := a.Reserve(ctx, commissionReq(t, "t1", "cx23", 180, 20))
	if err != nil || c.Status != Reserved {
		t.Fatalf("reserve = %+v, %v", c, err)
	}
	fw, srv, vol := c.Rows[0], c.Rows[1], c.Rows[2]
	for i, r := range c.Rows {
		if _, err := reg.SetLive(ctx, r.ID, itoa(int64(900+i))); err != nil {
			t.Fatal(err)
		}
	}
	a.SetAddress(ctx, srv.ID, "203.0.113.7")
	// created 70 minutes ago: two started hours
	db.ExecContext(ctx, `UPDATE cloud_resources SET created_at = now() - interval '70 minutes' WHERE hold_id = $1`, c.Hold.ID)
	acc, err := a.Accrue(ctx)
	wantServer, wantVolume := 2*0.0096, 2*1.144/730
	if err != nil || !near(acc.EUR, wantServer+wantVolume) || len(acc.Overruns) != 0 {
		t.Fatalf("accrued %+v, %v", acc, err)
	}
	if again, _ := a.Accrue(ctx); again.EUR != 0 {
		t.Fatalf("second pass booked %v", again)
	}
	reservation := 3 * (0.0096 + 1.144/730)
	if held := scalar[float64](t, db, `SELECT eur::float8 FROM holds WHERE id = $1`, c.Hold.ID); !near(held, reservation-wantServer-wantVolume) {
		t.Fatalf("held = %v", held)
	}
	if spent := scalar[float64](t, db, `SELECT spent_eur::float8 FROM tasks WHERE id = 't1'`); !near(spent, wantServer+wantVolume) {
		t.Fatalf("spent = %v", spent)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM ledger WHERE task_id = 't1' AND kind = 'resource' AND ref IN ($1, $2)`,
		"resource/"+itoa(srv.ID), "resource/"+itoa(vol.ID)); n != 2 {
		t.Fatalf("ledger rows = %d", n)
	}
	// the task's, the day's and the project's figures see booked + held = the reservation
	use, _ := a.Use(ctx, "sf/app")
	if !near(use.MonthEUR, reservation) {
		t.Fatalf("month use = %v, want %v", use.MonthEUR, reservation)
	}
	if addrs, _ := a.LiveAddresses(ctx); !reflect.DeepEqual(addrs, map[string][]string{"t1": {"203.0.113.7"}}) {
		t.Fatalf("addresses = %v", addrs)
	}

	// released after 70 minutes: nothing more to book, the rest returns
	for _, r := range []Resource{srv, fw, vol} {
		reg.Transition(ctx, r.ID, StateDeleting)
		reg.Transition(ctx, r.ID, StateDeleted)
	}
	db.ExecContext(ctx, `UPDATE cloud_resources SET deleted_at = created_at + interval '70 minutes' WHERE hold_id = $1`, c.Hold.ID)
	if acc, err := a.Accrue(ctx); err != nil || acc.EUR != 0 {
		t.Fatalf("final pass %+v, %v", acc, err)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM holds`); n != 0 {
		t.Fatalf("reservation left: %d", n)
	}
	if addrs, _ := a.LiveAddresses(ctx); len(addrs) != 0 {
		t.Fatalf("addresses after release = %v", addrs)
	}
	rows, _ := a.Owned(ctx, "t1")
	if len(rows) != 3 || rows[1].AccruedHours != 2 || !near(rows[1].AccruedEUR, wantServer) || rows[1].Address != "203.0.113.7" {
		t.Fatalf("owned = %+v", rows)
	}
}

// A create that failed leaves its requested row; the collector closes it
// (vanished, no provider id), and accrual books nothing for it and drops
// the reservation.
func TestFailedCreateRowIsClosedAndCostsNothing(t *testing.T) {
	db := agentDB(t)
	ctx := context.Background()
	seedTask(t, db, "t1", 1)
	a := AgentStore{DB: db}
	c, err := a.Reserve(ctx, commissionReq(t, "t1", "cx23", 60, 0))
	if err != nil {
		t.Fatal(err)
	}
	db.ExecContext(ctx, `UPDATE cloud_resources SET created_at = now() - interval '11 minutes' WHERE hold_id = $1`, c.Hold.ID)
	col := &Collector{Project: AgentProject, Registry: PG{DB: db}, Provider: newFakeProvider(), NodeIPs: testNodeIPs}
	if err := col.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	for _, r := range c.Rows {
		if got, _, _ := (PG{DB: db}).Get(ctx, r.ID); got.State != StateVanished {
			t.Fatalf("row %d = %s", r.ID, got.State)
		}
	}
	if acc, err := a.Accrue(ctx); err != nil || acc.EUR != 0 {
		t.Fatalf("accrued %+v, %v", acc, err)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM holds`); n != 0 {
		t.Fatalf("holds = %d", n)
	}
	if spent := scalar[float64](t, db, `SELECT spent_eur::float8 FROM tasks WHERE id = 't1'`); spent != 0 {
		t.Fatalf("spent = %v", spent)
	}
}

func TestSession(t *testing.T) {
	db := agentDB(t)
	ctx := context.Background()
	db.ExecContext(ctx, `DELETE FROM sessions WHERE task_id = 'ts'`)
	a := AgentStore{DB: db}
	if s, err := a.Session(ctx, "ts"); err != nil || s != "" {
		t.Fatalf("no session = %q, %v", s, err)
	}
	db.ExecContext(ctx, `INSERT INTO sessions (id, task_id, created_at) VALUES ('s-a', 'ts', now() - interval '1 hour'), ('s-b', 'ts', now())`)
	if s, err := a.Session(ctx, "ts"); err != nil || s != "s-b" {
		t.Fatalf("session = %q, %v", s, err)
	}
}

func TestPromoteAndEndLease(t *testing.T) {
	db := agentDB(t)
	ctx := context.Background()
	seedTask(t, db, "t1", 1)
	a := AgentStore{DB: db}
	c, err := a.Reserve(ctx, commissionReq(t, "t1", "cx23", 60, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Promote(ctx, c.Rows[1].ID, "sebastian"); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM cloud_resources WHERE hold_id = $1 AND lifetime = 'standing'`, c.Hold.ID); n != 2 {
		t.Fatalf("promoted rows = %d", n)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM holds`); n != 0 {
		t.Fatalf("reservation kept after promotion: %d", n)
	}
	if err := a.EndLease(ctx, c.Rows[0].ID, "sebastian"); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM cloud_resources WHERE hold_id = $1 AND released_by <> '' AND lease_expires <= now()`, c.Hold.ID); n != 2 {
		t.Fatalf("ended rows = %d", n)
	}
	// a released commission is not promoted back to life before the
	// collector deletes it
	if err := a.Promote(ctx, c.Rows[1].ID, "sebastian"); !errors.Is(err, ErrLeaseEnded) {
		t.Fatalf("promote after release: %v", err)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM cloud_resources WHERE hold_id = $1 AND lifetime = 'standing' AND released_by = ''`, c.Hold.ID); n != 0 {
		t.Fatalf("promoted after release: %d rows", n)
	}
	seedTask(t, db, "t9", 1)
	late, err := a.Reserve(ctx, commissionReq(t, "t9", "cx23", 60, 0))
	if err != nil {
		t.Fatal(err)
	}
	db.ExecContext(ctx, `UPDATE cloud_resources SET lease_expires = now() - interval '1 minute' WHERE hold_id = $1`, late.Hold.ID)
	if err := a.Promote(ctx, late.Rows[1].ID, "sebastian"); !errors.Is(err, ErrLeaseEnded) {
		t.Fatalf("promote after the lease ended: %v", err)
	}
	if err := a.Promote(ctx, 999999, "sebastian"); err == nil {
		t.Fatal("want an error for an unknown resource")
	}
	if err := a.EndLease(ctx, 999999, "sebastian"); err == nil {
		t.Fatal("want an error for an unknown resource")
	}
}

func TestWiden(t *testing.T) {
	// testEnvelope: 10 EUR, cx23 and cx33, 2 servers, 4 h, 20 GB
	for _, tc := range []struct {
		name        string
		use         EnvelopeUse
		typ         string
		minutes, gb int
		reservation float64
		want        policy.Envelope
	}{
		{"already fits: unchanged", EnvelopeUse{OpenServers: 1, MonthEUR: 1}, "cx23", 60, 0, 0.0096, testEnvelope},
		// 301 minutes are 6 started hours; 4 open servers need a 5th; 9.99 + 0.2345 = 10.2245 -> 10.23
		{"every bound", EnvelopeUse{OpenServers: 4, MonthEUR: 9.99}, "cx43", 301, 40, 0.2345,
			policy.Envelope{MonthlyEUR: 10.23, ServerTypes: []string{"cx23", "cx33", "cx43"}, MaxCount: 5, MaxLeaseHours: 6, VolumeMaxGB: 40,
				Standing: testEnvelope.Standing}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Widen(testEnvelope, tc.use, tc.typ, tc.minutes, tc.gb, tc.reservation)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Widen = %+v, want %+v", got, tc.want)
			}
			if v := EnvelopeViolations(got, tc.use, tc.typ, tc.minutes, tc.gb, tc.reservation); len(v) != 0 {
				t.Fatalf("still beyond the widened envelope: %v", v)
			}
		})
	}
	if len(testEnvelope.ServerTypes) != 2 {
		t.Fatal("Widen changed its argument's types")
	}
}

// The console's project approve widens the envelope as the human and
// approves the request in one go; the retry then fits the envelope.
func TestApproveForProjectAndEnvelopes(t *testing.T) {
	db := agentDB(t)
	ctx := context.Background()
	seedTask(t, db, "t1", 1)
	a := AgentStore{DB: db}
	req := commissionReq(t, "t1", "cx33", 300, 0) // 5 h > 4 h
	c, err := a.Reserve(ctx, req)
	if err != nil || c.Status != Pending {
		t.Fatalf("reserve = %+v, %v", c, err)
	}
	wide, err := a.ApproveForProject(ctx, c.ApprovalID, "sebastian")
	if err != nil || wide.MaxLeaseHours != 5 || wide.MonthlyEUR != 10 || wide.MaxCount != 2 {
		t.Fatalf("widened = %+v, %v", wide, err)
	}
	if st := scalar[string](t, db, `SELECT status || ' ' || decided_by || ' ' || consumed FROM approval_requests WHERE id = $1`, c.ApprovalID); st != "approved sebastian true" {
		t.Fatalf("request = %s", st)
	}
	envs, err := a.Envelopes(ctx)
	if err != nil || len(envs) != 1 || envs[0].Project != "sf/app" || envs[0].SetBy != "sebastian" || envs[0].MaxLeaseHours != 5 ||
		envs[0].Use.OpenServers != 0 {
		t.Fatalf("envelopes = %+v, %v", envs, err)
	}
	if _, err := a.ApproveForProject(ctx, c.ApprovalID, "sebastian"); err == nil {
		t.Fatal("a decided request was approved again")
	}
	ok, err := a.Reserve(ctx, req)
	if err != nil || ok.Status != Reserved {
		t.Fatalf("retry = %+v, %v", ok, err)
	}
	envs, _ = a.Envelopes(ctx)
	if envs[0].Use.OpenServers != 1 || !near(envs[0].Use.MonthEUR, req.Quote.ReservationEUR) {
		t.Fatalf("use = %+v", envs[0].Use)
	}
	// the project approval was spent on the widening: with the lease
	// narrowed again, the same request is beyond it and asks anew
	narrow := wide
	narrow.MaxLeaseHours, narrow.MonthlyEUR = 4, 100
	if err := a.SetEnvelope(ctx, "sf/app", narrow, "sebastian"); err != nil {
		t.Fatal(err)
	}
	again, err := a.Reserve(ctx, req)
	if err != nil || again.Status != Pending || again.ApprovalID == c.ApprovalID {
		t.Fatalf("after narrowing = %+v, %v; want a new pending request", again, err)
	}

	// the edit form: an existing project and a new one
	edit := policy.Envelope{MonthlyEUR: 25.5, ServerTypes: []string{"cx23"}, MaxCount: 1, MaxLeaseHours: 2, VolumeMaxGB: 0,
		Standing: policy.Standing{MonthlyEUR: 20, ServerTypes: []string{"cx23", "cx33"}, MaxCount: 1}}
	if err := a.SetEnvelope(ctx, "sf/app", edit, "sebastian"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetEnvelope(ctx, "sf/new", edit, "alice"); err != nil {
		t.Fatal(err)
	}
	envs, _ = a.Envelopes(ctx)
	if len(envs) != 2 || !reflect.DeepEqual(envs[0].Envelope, edit) || envs[1].Project != "sf/new" || envs[1].SetBy != "alice" {
		t.Fatalf("after edits = %+v", envs)
	}

	// the resources page lists open rows and those closed since, and who
	// promoted and released them
	if err := a.Promote(ctx, ok.Rows[1].ID, "sebastian"); err != nil {
		t.Fatal(err)
	}
	if err := a.EndLease(ctx, ok.Rows[1].ID, "alice"); err != nil {
		t.Fatal(err)
	}
	old, _ := a.Reserve(ctx, commissionReq(t, "t1", "cx23", 60, 0))
	db.ExecContext(ctx, `UPDATE cloud_resources SET state = 'deleted', deleted_at = now() - interval '8 days' WHERE hold_id = $1`, old.Hold.ID)
	rows, err := a.Commissions(ctx, time.Now().Add(-7*24*time.Hour))
	if err != nil || len(rows) != 2 || rows[1].Kind != KindServer || rows[1].PromotedBy != "sebastian" || rows[1].ReleasedBy != "alice" ||
		rows[1].Lifetime != LifetimeStanding {
		t.Fatalf("commissions = %+v, %v", rows, err)
	}
}

// ruling 124: a resource that outlives what its reservation paid for is
// booked in full and reported as an overrun, never clamped.
func TestAccrualPastTheReservationIsBookedAndReported(t *testing.T) {
	db := agentDB(t)
	ctx := context.Background()
	seedTask(t, db, "t1", 1)
	a := AgentStore{DB: db}
	c, err := a.Reserve(ctx, commissionReq(t, "t1", "cx23", 60, 0)) // reserves 1 h: 0.0096
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range c.Rows {
		(PG{DB: db}).SetLive(ctx, r.ID, itoa(int64(900+i)))
	}
	db.ExecContext(ctx, `UPDATE cloud_resources SET created_at = now() - interval '70 minutes' WHERE hold_id = $1`, c.Hold.ID)
	acc, err := a.Accrue(ctx)
	srv := c.Rows[1]
	if err != nil || !near(acc.EUR, 0.0192) || len(acc.Overruns) != 1 {
		t.Fatalf("accrual = %+v, %v", acc, err)
	}
	if o := acc.Overruns[0]; o.TaskID != "t1" || o.ResourceID != srv.ID || !near(o.EUR, 0.0192) || !near(o.HeldEUR, 0.0096) ||
		o.Reason != "COST OVER HOLD: resource "+itoa(srv.ID)+" cost 0.019200 EUR against a hold of 0.009600 EUR; booked in full" {
		t.Fatalf("overrun = %+v", o)
	}
	if spent := scalar[float64](t, db, `SELECT spent_eur::float8 FROM tasks WHERE id = 't1'`); !near(spent, 0.0192) {
		t.Fatalf("spent = %v", spent)
	}
	if held := scalar[float64](t, db, `SELECT eur::float8 FROM holds WHERE id = $1`, c.Hold.ID); held != 0 {
		t.Fatalf("held = %v", held)
	}
	// promoted: no reservation, never an overrun
	db.ExecContext(ctx, `UPDATE cloud_resources SET lifetime = 'standing', created_at = now() - interval '130 minutes' WHERE id = $1`, srv.ID)
	if acc, err := a.Accrue(ctx); err != nil || !near(acc.EUR, 0.0096) || len(acc.Overruns) != 0 {
		t.Fatalf("promoted accrual = %+v, %v", acc, err)
	}
	// m1: the standing hour is the project's line, not the task's spend
	if spent := scalar[float64](t, db, `SELECT spent_eur::float8 FROM tasks WHERE id = 't1'`); !near(spent, 0.0192) {
		t.Fatalf("spent after a standing hour = %v, want the leased hours only", spent)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM ledger WHERE task_id = 'standing:sf/app' AND kind = 'standing' AND ref = $1`, "resource/"+itoa(srv.ID)); n != 1 {
		t.Fatalf("standing ledger rows = %d", n)
	}
}

// A promoted commission the human later releases has no reservation left:
// its next hour, booked after the release, is no overrun.
func TestReleaseAfterPromoteIsNoOverrun(t *testing.T) {
	db := agentDB(t)
	ctx := context.Background()
	seedTask(t, db, "t1", 1)
	a := AgentStore{DB: db}
	c, err := a.Reserve(ctx, commissionReq(t, "t1", "cx23", 60, 0))
	if err != nil {
		t.Fatal(err)
	}
	srv := c.Rows[1]
	(PG{DB: db}).SetLive(ctx, srv.ID, "900")
	if err := a.Promote(ctx, srv.ID, "sebastian"); err != nil {
		t.Fatal(err)
	}
	if err := a.EndLease(ctx, srv.ID, "sebastian"); err != nil {
		t.Fatal(err)
	}
	// the collector's delete lands after the hour: two hours at 0.0096
	db.ExecContext(ctx, `UPDATE cloud_resources SET created_at = now() - interval '61 minutes' WHERE id = $1`, srv.ID)
	acc, err := a.Accrue(ctx)
	if err != nil || !near(acc.EUR, 0.0192) || len(acc.Overruns) != 0 {
		t.Fatalf("accrual = %+v, %v; want 0.0192 booked, no overrun", acc, err)
	}
}

// ruling 124: the egress drops a lease's address at the collector's cutoff.
func TestLiveAddressesEndAtTheCutoff(t *testing.T) {
	db := agentDB(t)
	ctx := context.Background()
	seedTask(t, db, "t1", 1)
	a := AgentStore{DB: db}
	c, err := a.Reserve(ctx, commissionReq(t, "t1", "cx23", 60, 0))
	if err != nil {
		t.Fatal(err)
	}
	srv := c.Rows[1]
	(PG{DB: db}).SetLive(ctx, srv.ID, "900")
	a.SetAddress(ctx, srv.ID, "203.0.113.7")
	for _, tc := range []struct {
		left string
		want int
	}{{"211 seconds", 1}, {"209 seconds", 0}} { // the cutoff: 2 min + 90 s = 210 s
		db.ExecContext(ctx, `UPDATE cloud_resources SET lease_expires = now() + $2::interval WHERE id = $1`, srv.ID, tc.left)
		if addrs, _ := a.LiveAddresses(ctx); len(addrs["t1"]) != tc.want {
			t.Fatalf("lease ending in %s: addresses %v", tc.left, addrs)
		}
	}
}

// An approval covers only the ways it was beyond; a task has at
// most MaxPendingRequests pending.
func TestApprovalCoversItsOwnViolationsAndPendingIsCapped(t *testing.T) {
	db := agentDB(t)
	ctx := context.Background()
	seedTask(t, db, "t1", 5)
	a := AgentStore{DB: db}
	long := commissionReq(t, "t1", "cx33", 300, 0) // lease beyond
	c, err := a.Reserve(ctx, long)
	if err != nil || c.Status != Pending {
		t.Fatalf("first = %+v, %v", c, err)
	}
	if k := scalar[string](t, db, `SELECT spec->>'beyond_kinds' FROM approval_requests WHERE id = $1`, c.ApprovalID); k != `["lease"]` {
		t.Fatalf("beyond_kinds = %s", k)
	}
	db.ExecContext(ctx, `UPDATE approval_requests SET status = 'approved' WHERE id = $1`, c.ApprovalID)
	// meanwhile the project fills its two servers: the approval covered
	// the lease only, so the count is a new request
	seedTask(t, db, "t2", 5)
	for range 2 {
		if r, err := a.Reserve(ctx, commissionReq(t, "t2", "cx23", 60, 0)); err != nil || r.Status != Reserved {
			t.Fatalf("filler = %+v, %v", r, err)
		}
	}
	again, err := a.Reserve(ctx, long)
	if err != nil || again.Status != Pending || again.ApprovalID == c.ApprovalID {
		t.Fatalf("after the count grew = %+v, %v", again, err)
	}
	// two more distinct requests reach the cap of 3 pending; a fourth is refused
	for _, m := range []int{310, 400} {
		if r, err := a.Reserve(ctx, commissionReq(t, "t1", "cx23", m, 0)); err != nil || r.Status != Pending {
			t.Fatalf("pending %d = %+v, %v", m, r, err)
		}
	}
	_, err = a.Reserve(ctx, commissionReq(t, "t1", "cx23", 500, 0))
	var ref *policy.Refusal
	if !errors.As(err, &ref) || ref.Decision != "refused_approvals" || !strings.Contains(ref.Reason, "3 pending resource requests (at most 3)") {
		t.Fatalf("fourth = %v", err)
	}
	// a request a pending one covers is still answered with it at the cap
	if r, err := a.Reserve(ctx, commissionReq(t, "t1", "cx33", 60, 0)); err != nil || r.Status != Pending || r.ApprovalID != again.ApprovalID {
		t.Fatalf("repeat = %+v, %v", r, err)
	}
}

// An overrun is the row's that overran, measured against its own share of
// the reservation: a cx23 for 60 minutes with 20 GB reserves 0.0096 for the
// server and 1.144/730 = 0.0015671 for the volume. The server lives into
// its second hour (2 x 0.0096 = 0.0192 against its 0.0096), the volume
// stays in its first (0.0015671, its whole share): one overrun, the
// server's, and the volume's share is still held for it.
func TestOverrunIsTheRowThatOverran(t *testing.T) {
	db := agentDB(t)
	ctx := context.Background()
	seedTask(t, db, "t1", 1)
	a := AgentStore{DB: db}
	c, err := a.Reserve(ctx, commissionReq(t, "t1", "cx23", 60, 20))
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range c.Rows {
		(PG{DB: db}).SetLive(ctx, r.ID, itoa(int64(900+i)))
	}
	srv, vol := c.Rows[1], c.Rows[2]
	db.ExecContext(ctx, `UPDATE cloud_resources SET created_at = now() - interval '70 minutes' WHERE id = $1`, srv.ID)
	db.ExecContext(ctx, `UPDATE cloud_resources SET created_at = now() - interval '30 minutes' WHERE id = $1`, vol.ID)
	acc, err := a.Accrue(ctx)
	if err != nil || !near(acc.EUR, 0.0192+1.144/730) || len(acc.Overruns) != 1 {
		t.Fatalf("accrual = %+v, %v", acc, err)
	}
	if o := acc.Overruns[0]; o.ResourceID != srv.ID || !near(o.EUR, 0.0192) || !near(o.HeldEUR, 0.0096) {
		t.Fatalf("overrun = %+v", o)
	}
	if held := scalar[float64](t, db, `SELECT eur::float8 FROM holds WHERE id = $1`, c.Hold.ID); !near(held, 0) {
		t.Fatalf("held = %v", held)
	}
}

// A commissioned server's commissioning, from its request to its live
// moment, is claimed for its task's timeline once.
func TestClaimCommissionedOncePerServer(t *testing.T) {
	db := agentDB(t)
	ctx := context.Background()
	seedTask(t, db, "t1", 1)
	a := AgentStore{DB: db}
	c, err := a.Reserve(ctx, commissionReq(t, "t1", "cx23", 60, 0))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := a.ClaimCommissioned(ctx); err != nil || len(got) != 0 {
		t.Fatalf("before the server is live: %+v, %v", got, err)
	}
	srv := c.Rows[1]
	(PG{DB: db}).SetLive(ctx, c.Rows[0].ID, "fw-1")
	(PG{DB: db}).SetLive(ctx, srv.ID, "900")
	db.ExecContext(ctx, `UPDATE cloud_resources SET created_at = live_at - interval '95 seconds' WHERE id = $1`, srv.ID)
	got, err := a.ClaimCommissioned(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("claimed %+v, %v; want the one server", got, err)
	}
	if g := got[0]; g.ID != srv.ID || g.TaskID != "t1" || g.ServerType != "cx23" || g.Live.Sub(g.Requested) != 95*time.Second {
		t.Fatalf("claimed %+v, want server %d of t1, cx23, live 95 s after its request", g, srv.ID)
	}
	if again, _ := a.ClaimCommissioned(ctx); len(again) != 0 {
		t.Fatalf("claimed twice: %+v", again)
	}
}

// ruling 147: a commission becomes standing only within the project's
// standing block: its server type, one more standing server, its monthly
// price on top of the standing ones.
func TestPromoteWithinStandingBlock(t *testing.T) {
	db := agentDB(t)
	ctx := context.Background()
	seedTask(t, db, "t1", 5)
	a := AgentStore{DB: db}
	first, err := a.Reserve(ctx, commissionReq(t, "t1", "cx23", 60, 0))
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Reserve(ctx, commissionReq(t, "t1", "cx23", 60, 0))
	if err != nil {
		t.Fatal(err)
	}
	monthly := first.Rows[1].MonthlyEUR + first.Rows[0].MonthlyEUR
	env, _, err := a.Envelope(ctx, "sf/app")
	if err != nil {
		t.Fatal(err)
	}
	env.Standing = policy.Standing{ServerTypes: []string{}}
	a.SetEnvelope(ctx, "sf/app", env, "sebastian")
	if err := a.Promote(ctx, first.Rows[1].ID, "sebastian"); !errors.Is(err, ErrBeyondStanding) {
		t.Fatalf("promote with a closed standing block = %v, want ErrBeyondStanding", err)
	}
	env.Standing = policy.Standing{MonthlyEUR: monthly, ServerTypes: []string{"cx33"}, MaxCount: 2}
	a.SetEnvelope(ctx, "sf/app", env, "sebastian")
	if err := a.Promote(ctx, first.Rows[1].ID, "sebastian"); !errors.Is(err, ErrBeyondStanding) || !strings.Contains(err.Error(), "cx23") {
		t.Fatalf("promote of a type outside the block = %v", err)
	}
	env.Standing.ServerTypes = []string{"cx23"}
	a.SetEnvelope(ctx, "sf/app", env, "sebastian")
	if err := a.Promote(ctx, first.Rows[1].ID, "sebastian"); err != nil {
		t.Fatalf("promote within the block: %v", err)
	}
	if err := a.Promote(ctx, second.Rows[1].ID, "sebastian"); !errors.Is(err, ErrBeyondStanding) || !strings.Contains(err.Error(), "EUR") {
		t.Fatalf("promote past the monthly line = %v", err)
	}
	envs, err := a.Envelopes(ctx)
	if err != nil || len(envs) != 1 || envs[0].StandingUse.Count != 1 || math.Abs(envs[0].StandingUse.MonthlyEUR-monthly) > 1e-9 {
		t.Fatalf("envelopes = %+v, %v; want one standing server at %.4f EUR a month", envs, err, monthly)
	}
	// a standing server leaves the task's egress: no agent SSH (ruling 147)
	db.ExecContext(ctx, `UPDATE cloud_resources SET state = 'live', address = '192.0.2.1' WHERE hold_id = $1 AND kind = 'server'`, first.Hold.ID)
	if addrs, err := a.LiveAddresses(ctx); err != nil || len(addrs["t1"]) != 0 {
		t.Fatalf("live addresses = %v, %v; want none for a standing server", addrs, err)
	}
}
