package cloudres

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/hcloud"
)

// fakeProvider is an in-memory Hetzner project.
type fakeProvider struct {
	mu        sync.Mutex
	servers   map[int64]hcloud.Server
	firewalls map[int64]hcloud.Firewall
	deleted   []string
	inUse     map[int64]bool // firewall ids answering resource_in_use
	listLag   map[int64]bool // server ids missing from listings but present on GET
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{servers: map[int64]hcloud.Server{}, firewalls: map[int64]hcloud.Firewall{},
		inUse: map[int64]bool{}, listLag: map[int64]bool{}}
}

func (f *fakeProvider) Servers(ctx context.Context, sel string) ([]hcloud.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []hcloud.Server
	for id, s := range f.servers {
		if !f.listLag[id] {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b hcloud.Server) int { return cmp.Compare(a.ID, b.ID) }) // the API lists by id
	return out, nil
}

func (f *fakeProvider) Server(ctx context.Context, id int64) (hcloud.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[id]
	if !ok {
		return s, &hcloud.APIError{Status: 404, Code: "not_found"}
	}
	return s, nil
}

func (f *fakeProvider) DeleteServer(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.servers, id)
	f.deleted = append(f.deleted, "server/"+itoa(id))
	return nil
}

func (f *fakeProvider) Firewalls(ctx context.Context, sel string) ([]hcloud.Firewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []hcloud.Firewall
	for _, fw := range f.firewalls {
		out = append(out, fw)
	}
	slices.SortFunc(out, func(a, b hcloud.Firewall) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

func (f *fakeProvider) Firewall(ctx context.Context, id int64) (hcloud.Firewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fw, ok := f.firewalls[id]
	if !ok {
		return fw, &hcloud.APIError{Status: 404, Code: "not_found"}
	}
	return fw, nil
}

func (f *fakeProvider) DeleteFirewall(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inUse[id] {
		return &hcloud.APIError{Status: 409, Code: "resource_in_use"}
	}
	delete(f.firewalls, id)
	f.deleted = append(f.deleted, "firewall/"+itoa(id))
	return nil
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

var t0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

type harness struct {
	reg  *Memory
	prov *fakeProvider
	clk  *clock
	done map[string]bool
	col  *Collector
}

func newHarness() *harness {
	h := &harness{reg: NewMemory(), prov: newFakeProvider(), clk: &clock{t: t0}, done: map[string]bool{}}
	h.reg.Now = h.clk.now
	h.col = &Collector{Project: "ci", Registry: h.reg, Provider: h.prov, Now: h.clk.now,
		OwnerDone: func(ctx context.Context, owner string) (bool, error) { return h.done[owner], nil }}
	return h
}

// create mimics the owner's create path: row first, then the provider
// resource carrying the row's labels, then the provider id.
func (h *harness) create(t *testing.T, kind, owner string, providerID int64, lease time.Duration) Resource {
	t.Helper()
	r, err := h.reg.Insert(context.Background(), Resource{Project: "ci", Kind: kind, Name: kind + "-" + owner,
		OwnerRef: owner, LeaseExpires: h.clk.t.Add(lease), Labels: map[string]string{"job": owner}})
	if err != nil {
		t.Fatal(err)
	}
	if kind == KindServer {
		h.prov.servers[providerID] = hcloud.Server{ID: providerID, Labels: r.Labels}
	} else {
		h.prov.firewalls[providerID] = hcloud.Firewall{ID: providerID, Labels: r.Labels}
	}
	if _, err := h.reg.SetLive(context.Background(), r.ID, itoa(providerID)); err != nil {
		t.Fatal(err)
	}
	r, _ = h.reg.Get(r.ID)
	return r
}

func (h *harness) state(t *testing.T, id int64) string {
	t.Helper()
	r, ok := h.reg.Get(id)
	if !ok {
		t.Fatalf("row %d missing", id)
	}
	return r.State
}

func TestInsertAddsProviderLabels(t *testing.T) {
	reg := NewMemory()
	r, err := reg.Insert(context.Background(), Resource{Project: "ci", Kind: KindServer, OwnerRef: "7",
		Labels: map[string]string{"job": "7"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"factory": "ci", "job": "7", "resource": "1"}
	if !maps.Equal(r.Labels, want) || r.State != StateRequested {
		t.Fatalf("row = %+v", r)
	}
}

func TestStateMachine(t *testing.T) {
	testStateMachine(t, NewMemory())
}

func testStateMachine(t *testing.T, reg Registry) {
	ctx := context.Background()
	insert := func() int64 {
		r, err := reg.Insert(ctx, Resource{Project: "ci", Kind: KindServer, Name: "s", OwnerRef: "1", LeaseExpires: t0})
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	step := func(id int64, to string, want bool) {
		t.Helper()
		var got bool
		var err error
		if to == StateLive {
			got, err = reg.SetLive(ctx, id, "99")
		} else {
			got, err = reg.Transition(ctx, id, to)
		}
		if err != nil || got != want {
			t.Fatalf("row %d -> %s = %v, %v, want %v", id, to, got, err, want)
		}
	}

	a := insert() // the normal life: requested, live, deleting, deleted
	step(a, StateDeleted, true)
	step(a, StateLive, false)
	step(a, StateDeleting, false)
	step(a, StateDeleted, false)

	b := insert()
	step(b, StateLive, true)
	step(b, StateLive, false)
	step(b, StateDeleted, false) // a live resource is deleted through deleting
	step(b, StateDeleting, true)
	step(b, StateDeleting, false)
	step(b, StateDeleted, true)
	step(b, StateVanished, false)

	c := insert()
	step(c, StateLive, true)
	step(c, StateVanished, true)
	step(c, StateDeleting, false)

	if _, err := reg.Transition(ctx, c, "bogus"); err == nil {
		t.Fatal("want an error for an unknown state")
	}
	open, err := reg.Open(ctx, "ci")
	if err != nil || len(open) != 0 {
		t.Fatalf("open = %+v, %v, want none", open, err)
	}
	d := insert()
	step(d, StateLive, true)
	open, err = reg.OpenByOwner(ctx, "ci", "1")
	if err != nil || len(open) != 1 || open[0].ID != d || open[0].ProviderID != "99" || open[0].State != StateLive {
		t.Fatalf("open by owner = %+v, %v", open, err)
	}
	if other, _ := reg.OpenByOwner(ctx, "agent", "1"); len(other) != 0 {
		t.Fatalf("other project sees %+v", other)
	}
}

func TestCollectKeepsLiveLeasedResources(t *testing.T) {
	h := newHarness()
	s := h.create(t, KindServer, "5", 100, time.Hour)
	f := h.create(t, KindFirewall, "5", 200, time.Hour)
	if err := h.col.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.prov.deleted) != 0 || h.state(t, s.ID) != StateLive || h.state(t, f.ID) != StateLive {
		t.Fatalf("deleted %v", h.prov.deleted)
	}
}

func TestCollectDeletesExpiredLeaseServersFirst(t *testing.T) {
	h := newHarness()
	f := h.create(t, KindFirewall, "5", 200, time.Hour)
	s := h.create(t, KindServer, "5", 100, time.Hour)
	h.clk.t = t0.Add(time.Hour)
	if err := h.col.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.prov.deleted, ",") != "server/100,firewall/200" {
		t.Fatalf("deleted = %v", h.prov.deleted)
	}
	if h.state(t, s.ID) != StateDeleted || h.state(t, f.ID) != StateDeleted {
		t.Fatalf("states = %s %s", h.state(t, s.ID), h.state(t, f.ID))
	}
}

func TestCollectDeletesFinishedOwner(t *testing.T) {
	h := newHarness()
	s := h.create(t, KindServer, "5", 100, time.Hour)
	keep := h.create(t, KindServer, "6", 101, time.Hour)
	h.done["5"] = true
	if err := h.col.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.prov.deleted, ",") != "server/100" || h.state(t, s.ID) != StateDeleted || h.state(t, keep.ID) != StateLive {
		t.Fatalf("deleted = %v", h.prov.deleted)
	}
}

func TestCollectDeletesUnknownAndClosedResources(t *testing.T) {
	h := newHarness()
	h.prov.servers[300] = hcloud.Server{ID: 300}                                                                   // made by hand, no labels
	h.prov.firewalls[301] = hcloud.Firewall{ID: 301, Labels: map[string]string{"factory": "ci", "resource": "42"}} // no such row
	old := h.create(t, KindServer, "5", 302, time.Hour)
	if _, err := h.reg.Transition(context.Background(), old.ID, StateDeleting); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reg.Transition(context.Background(), old.ID, StateDeleted); err != nil {
		t.Fatal(err)
	}
	// row deleted, server somehow still there: known as deleted = unknown
	if err := h.col.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.prov.deleted, ",") != "server/300,server/302,firewall/301" {
		t.Fatalf("deleted = %v", h.prov.deleted)
	}
}

func TestCollectMarksVanished(t *testing.T) {
	h := newHarness()
	gone := h.create(t, KindServer, "5", 100, time.Hour)
	delete(h.prov.servers, 100)
	lagging := h.create(t, KindServer, "5", 101, time.Hour)
	h.prov.listLag[101] = true // absent from the listing, still there on GET
	neverMade, _ := h.reg.Insert(context.Background(), Resource{Project: "ci", Kind: KindServer, OwnerRef: "5", LeaseExpires: t0.Add(time.Hour)})
	h.clk.t = t0.Add(5 * time.Minute)
	if err := h.col.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.state(t, gone.ID) != StateVanished || h.state(t, lagging.ID) != StateLive || h.state(t, neverMade.ID) != StateRequested {
		t.Fatalf("states = %s %s %s", h.state(t, gone.ID), h.state(t, lagging.ID), h.state(t, neverMade.ID))
	}
	h.clk.t = t0.Add(11 * time.Minute)
	if err := h.col.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.state(t, neverMade.ID) != StateVanished {
		t.Fatalf("never-made row = %s", h.state(t, neverMade.ID))
	}
	if len(h.prov.deleted) != 0 {
		t.Fatalf("deleted = %v", h.prov.deleted)
	}
}

func TestCollectAdoptsRequestedRowByLabel(t *testing.T) {
	h := newHarness()
	r, _ := h.reg.Insert(context.Background(), Resource{Project: "ci", Kind: KindServer, OwnerRef: "5", LeaseExpires: t0.Add(time.Hour)})
	h.prov.servers[100] = hcloud.Server{ID: 100, Labels: r.Labels} // created, provider id never recorded
	if err := h.col.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := h.reg.Get(r.ID)
	if got.State != StateLive || got.ProviderID != "100" || len(h.prov.deleted) != 0 {
		t.Fatalf("row = %+v, deleted %v", got, h.prov.deleted)
	}
}

func TestCollectRetriesFirewallInUseAndClosesOn404(t *testing.T) {
	h := newHarness()
	f := h.create(t, KindFirewall, "5", 200, time.Hour)
	h.prov.inUse[200] = true
	h.done["5"] = true
	err := h.col.Collect(context.Background())
	if !hcloud.IsCode(err, "resource_in_use") || h.state(t, f.ID) != StateDeleting {
		t.Fatalf("err = %v, state %s", err, h.state(t, f.ID))
	}
	h.prov.inUse[200] = false
	if err := h.col.Collect(context.Background()); err != nil || h.state(t, f.ID) != StateDeleted {
		t.Fatalf("err = %v, state %s", err, h.state(t, f.ID))
	}

	// a deleting row whose resource is already gone closes as deleted
	g := h.create(t, KindFirewall, "6", 201, time.Hour)
	if _, err := h.reg.Transition(context.Background(), g.ID, StateDeleting); err != nil {
		t.Fatal(err)
	}
	delete(h.prov.firewalls, 201)
	if err := h.col.Collect(context.Background()); err != nil || h.state(t, g.ID) != StateDeleted {
		t.Fatalf("err = %v, state %s", err, h.state(t, g.ID))
	}
}

func TestCollectIgnoresOtherProjectRows(t *testing.T) {
	h := newHarness()
	r, _ := h.reg.Insert(context.Background(), Resource{Project: "agent", Kind: KindServer, OwnerRef: "t", LeaseExpires: t0})
	h.prov.servers[100] = hcloud.Server{ID: 100, Labels: r.Labels}
	if err := h.col.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	// the ci collector owns the ci project: an agent-labelled server here is unknown
	if strings.Join(h.prov.deleted, ",") != "server/100" {
		t.Fatalf("deleted = %v", h.prov.deleted)
	}
	if got, _ := h.reg.Get(r.ID); got.State != StateRequested {
		t.Fatalf("agent row touched: %+v", got)
	}
}

func TestOwnerErrorSkipsRow(t *testing.T) {
	h := newHarness()
	h.create(t, KindServer, "5", 100, time.Hour)
	h.col.OwnerDone = func(ctx context.Context, owner string) (bool, error) { return false, errors.New("forgejo down") }
	if err := h.col.Collect(context.Background()); err == nil || len(h.prov.deleted) != 0 {
		t.Fatalf("err = %v, deleted %v", err, h.prov.deleted)
	}
}

// Requires a disposable Postgres: FACTORYD_TEST_DATABASE_URL.
func TestPGStateMachine(t *testing.T) {
	dsn := os.Getenv("FACTORYD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FACTORYD_TEST_DATABASE_URL not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	files, _ := filepath.Glob("../factoryd/migrations/*_cloud_resources.sql")
	if len(files) != 1 {
		t.Fatalf("migration files = %v", files)
	}
	schema, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS cloud_resources`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range strings.Split(string(schema), ";") {
		if strings.TrimSpace(stmt) != "" {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Fatal(err)
			}
		}
	}
	reg := PG{DB: db}
	testStateMachine(t, reg)

	r, err := reg.Insert(ctx, Resource{Project: "ci", Kind: KindFirewall, Name: "fw", OwnerRef: "8",
		PricePerHourEUR: 0.0096, LeaseExpires: t0, Labels: map[string]string{"job": "8"}})
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := reg.Get(ctx, r.ID)
	if err != nil || !ok {
		t.Fatalf("get = %v, %v", ok, err)
	}
	if got.Labels["resource"] != itoa(r.ID) || got.Labels["factory"] != "ci" || got.Labels["job"] != "8" ||
		got.PricePerHourEUR != 0.0096 || !got.LeaseExpires.Equal(t0) || got.State != StateRequested || got.DeletedAt != nil {
		t.Fatalf("row = %+v", got)
	}
	if _, err := reg.Transition(ctx, r.ID, StateDeleted); err != nil {
		t.Fatal(err)
	}
	if got, _, _ = reg.Get(ctx, r.ID); got.DeletedAt == nil {
		t.Fatal("deleted_at not stamped")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO cloud_resources (project, kind, name, owner_ref, lease_expires) VALUES ('other', 'server', 'x', '1', now())`); err == nil {
		t.Fatal("want the project check to refuse 'other'")
	}
}
