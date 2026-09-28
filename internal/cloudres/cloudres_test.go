package cloudres

import (
	"cmp"
	"context"
	"errors"
	"maps"
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
	volumes   map[int64]hcloud.Volume
	ips       map[int64]hcloud.PrimaryIP
	deleted   []string
	inUse     map[int64]bool // firewall ids answering resource_in_use
	listLag   map[int64]bool // server ids missing from listings but present on GET

	// With clk set, every call takes callTook on it and a server delete
	// completes deleteTook after its call: until then the server exists,
	// holds its volumes and refuses their detach (locked), as Hetzner's.
	clk        *clock
	callTook   time.Duration
	deleteTook time.Duration
	goneAt     map[int64]time.Time
	calledAt   map[string]time.Time // delete calls by "kind/id"
	// detachRefusal answers a detach from a deleting server; nil = locked
	detachRefusal *hcloud.APIError
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{servers: map[int64]hcloud.Server{}, firewalls: map[int64]hcloud.Firewall{},
		volumes: map[int64]hcloud.Volume{}, ips: map[int64]hcloud.PrimaryIP{},
		inUse: map[int64]bool{}, listLag: map[int64]bool{}, goneAt: map[int64]time.Time{}, calledAt: map[string]time.Time{}}
}

// call models one API call's latency and completes due server deletes.
func (f *fakeProvider) call() {
	if f.clk == nil {
		return
	}
	f.clk.t = f.clk.t.Add(f.callTook)
	for id, at := range f.goneAt {
		if !f.clk.t.Before(at) {
			f.removeServer(id)
			delete(f.goneAt, id)
		}
	}
}

func (f *fakeProvider) did(name string) {
	f.deleted = append(f.deleted, name)
	if f.clk != nil {
		f.calledAt[name] = f.clk.t
	}
}

func (f *fakeProvider) Servers(ctx context.Context, sel string) ([]hcloud.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call()
	var out []hcloud.Server
	for id, s := range f.servers {
		if !f.listLag[id] && selects(sel, s.Labels) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b hcloud.Server) int { return cmp.Compare(a.ID, b.ID) }) // the API lists by id
	return out, nil
}

func (f *fakeProvider) Server(ctx context.Context, id int64) (hcloud.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call()
	s, ok := f.servers[id]
	if !ok {
		return s, &hcloud.APIError{Status: 404, Code: "not_found"}
	}
	return s, nil
}

// DeleteServer detaches the server's volumes and deletes the primary IPs
// assigned to it, as Hetzner does (auto_delete).
func (f *fakeProvider) DeleteServer(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call()
	f.did("server/" + itoa(id))
	if f.deleteTook > 0 {
		f.goneAt[id] = f.clk.t.Add(f.deleteTook)
		return nil
	}
	f.removeServer(id)
	return nil
}

func (f *fakeProvider) removeServer(id int64) {
	delete(f.servers, id)
	for vid, v := range f.volumes {
		if v.Server != nil && *v.Server == id {
			v.Server = nil
			f.volumes[vid] = v
		}
	}
	for ipID, ip := range f.ips {
		if ip.AssigneeID != nil && *ip.AssigneeID == id {
			delete(f.ips, ipID)
		}
	}
}

func (f *fakeProvider) Volumes(ctx context.Context, sel string) ([]hcloud.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call()
	var out []hcloud.Volume
	for _, v := range f.volumes {
		if selects(sel, v.Labels) {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b hcloud.Volume) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

func (f *fakeProvider) Volume(ctx context.Context, id int64) (hcloud.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call()
	v, ok := f.volumes[id]
	if !ok {
		return v, &hcloud.APIError{Status: 404, Code: "not_found"}
	}
	return v, nil
}

func (f *fakeProvider) DetachVolume(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call()
	v := f.volumes[id]
	if v.Server != nil {
		if _, deleting := f.goneAt[*v.Server]; deleting {
			if f.detachRefusal != nil {
				return f.detachRefusal
			}
			return &hcloud.APIError{Status: 423, Code: "locked"}
		}
	}
	v.Server = nil
	f.volumes[id] = v
	f.deleted = append(f.deleted, "detach/"+itoa(id))
	return nil
}

func (f *fakeProvider) DeleteVolume(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call()
	if v, ok := f.volumes[id]; ok && v.Server != nil {
		return &hcloud.APIError{Status: 423, Code: "locked"}
	}
	delete(f.volumes, id)
	f.did("volume/" + itoa(id))
	return nil
}

func (f *fakeProvider) PrimaryIPs(ctx context.Context, sel string) ([]hcloud.PrimaryIP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call()
	out := slices.Collect(maps.Values(f.ips))
	slices.SortFunc(out, func(a, b hcloud.PrimaryIP) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

func (f *fakeProvider) DeletePrimaryIP(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call()
	delete(f.ips, id)
	f.did("primary_ip/" + itoa(id))
	return nil
}

func (f *fakeProvider) Firewalls(ctx context.Context, sel string) ([]hcloud.Firewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call()
	var out []hcloud.Firewall
	for _, fw := range f.firewalls {
		if selects(sel, fw.Labels) {
			out = append(out, fw)
		}
	}
	slices.SortFunc(out, func(a, b hcloud.Firewall) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

func (f *fakeProvider) Firewall(ctx context.Context, id int64) (hcloud.Firewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call()
	fw, ok := f.firewalls[id]
	if !ok {
		return fw, &hcloud.APIError{Status: 404, Code: "not_found"}
	}
	return fw, nil
}

func (f *fakeProvider) DeleteFirewall(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call()
	if f.inUse[id] {
		return &hcloud.APIError{Status: 409, Code: "resource_in_use"}
	}
	delete(f.firewalls, id)
	f.did("firewall/" + itoa(id))
	return nil
}

// selects is a label selector of the one form the code sends: k=v.
func selects(sel string, labels map[string]string) bool {
	if sel == "" {
		return true
	}
	k, v, _ := strings.Cut(sel, "=")
	return labels[k] == v
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
	h.col = &Collector{Project: "ci", Registry: h.reg, Provider: h.prov, Now: h.clk.now, NodeIPs: testNodeIPs, RetryWait: time.Millisecond,
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
	switch kind {
	case KindServer:
		h.prov.servers[providerID] = hcloud.Server{ID: providerID, Labels: r.Labels}
	case KindFirewall:
		h.prov.firewalls[providerID] = hcloud.Firewall{ID: providerID, Labels: r.Labels}
	case KindVolume:
		h.prov.volumes[providerID] = hcloud.Volume{ID: providerID, Labels: r.Labels}
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

var testNodeIPs = []string{"198.51.100.10/32", "198.51.100.11/32"}

func serverAt(id int64, ip string, labels map[string]string) hcloud.Server {
	s := hcloud.Server{ID: id, Labels: labels}
	s.PublicNet.IPv4 = &struct {
		IP string `json:"ip"`
	}{IP: ip}
	return s
}

// Ruling 123: a pass that sees a cluster node or another project's server
// holds the wrong token; it deletes nothing, not even what it would
// otherwise collect.
func TestCollectRefusesAPassOverTheWrongProject(t *testing.T) {
	for name, wrong := range map[string]hcloud.Server{
		"a node's IPv4, no labels":    serverAt(900, "198.51.100.11", nil),
		"a node's IPv4 as /32 source": serverAt(900, "198.51.100.10", map[string]string{"factory": "ci"}),
		"another project's label":     serverAt(900, "203.0.113.9", map[string]string{"factory": "agent", "resource": "1"}),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			h.prov.servers[900] = wrong
			h.prov.servers[300] = serverAt(300, "203.0.113.30", nil) // hand-made: would be deleted
			expired := h.create(t, KindServer, "5", 301, time.Minute)
			h.clk.t = t0.Add(time.Hour)
			err := h.col.Collect(context.Background())
			if err == nil || !strings.Contains(err.Error(), "refusing the pass") {
				t.Fatalf("err = %v, want a refused pass", err)
			}
			if len(h.prov.deleted) != 0 {
				t.Fatalf("deleted = %v, want nothing", h.prov.deleted)
			}
			if h.state(t, expired.ID) != StateLive {
				t.Fatalf("expired row = %s, want untouched", h.state(t, expired.ID))
			}
		})
	}
}

// Any listed kind with another project's label is the wrong token, even
// with no server in sight (ruling 123).
func TestCollectRefusesAForeignLabelOnAnyKind(t *testing.T) {
	for _, kind := range []string{KindFirewall, KindVolume} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness()
			foreign := map[string]string{"factory": "agent", "resource": "7"}
			if kind == KindFirewall {
				h.prov.firewalls[900] = hcloud.Firewall{ID: 900, Labels: foreign}
				h.prov.firewalls[901] = hcloud.Firewall{ID: 901} // would be deleted as unknown
			} else {
				h.prov.volumes[900] = hcloud.Volume{ID: 900, Labels: foreign}
				h.prov.firewalls[901] = hcloud.Firewall{ID: 901}
			}
			err := h.col.Collect(context.Background())
			if err == nil || !strings.Contains(err.Error(), "refusing the pass") {
				t.Fatalf("err = %v, want a refused pass", err)
			}
			if len(h.prov.deleted) != 0 {
				t.Fatalf("deleted = %v, want nothing", h.prov.deleted)
			}
		})
	}
}

// Without the node IPs to compare with, the interlock cannot hold: refuse.
func TestCollectRefusesWithoutNodeIPs(t *testing.T) {
	h := newHarness()
	h.col.NodeIPs = nil
	h.prov.servers[300] = serverAt(300, "203.0.113.30", nil)
	if err := h.col.Collect(context.Background()); err == nil || !strings.Contains(err.Error(), "refusing the pass") {
		t.Fatalf("err = %v, want a refused pass", err)
	}
	if len(h.prov.deleted) != 0 {
		t.Fatalf("deleted = %v", h.prov.deleted)
	}
}

// A hand-made unlabelled server off the node IPs is still collected (proof e).
func TestCollectDeletesHandMadeServerOffTheNodes(t *testing.T) {
	h := newHarness()
	h.prov.servers[300] = serverAt(300, "203.0.113.30", nil)
	h.prov.servers[301] = serverAt(301, "203.0.113.31", map[string]string{"factory": "ci"})
	if err := h.col.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.prov.deleted, ",") != "server/300,server/301" {
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
	// same resource id, no factory label: only the ci registry decides
	h.prov.servers[100] = hcloud.Server{ID: 100, Labels: map[string]string{"resource": r.Labels["resource"]}}
	if err := h.col.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	// the ci collector owns the ci project: a server its registry does not know is unknown
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
	db := agentDB(t)
	ctx := context.Background()
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

func ptr(v int64) *int64 { return &v }

func TestCollectKeepsPromotedPastLeaseAndOwner(t *testing.T) {
	h := newHarness()
	r, _ := h.reg.Insert(context.Background(), Resource{Project: "ci", Kind: KindServer, OwnerRef: "5",
		LeaseExpires: t0.Add(time.Minute), Promoted: true})
	h.prov.servers[100] = hcloud.Server{ID: 100, Labels: r.Labels}
	h.reg.SetLive(context.Background(), r.ID, "100")
	h.done["5"] = true
	h.clk.t = t0.Add(time.Hour)
	if err := h.col.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.prov.deleted) != 0 || h.state(t, r.ID) != StateLive {
		t.Fatalf("promoted resource collected: deleted %v, state %s", h.prov.deleted, h.state(t, r.ID))
	}
}

func TestCollectDeletesVolumeAfterItsServer(t *testing.T) {
	h := newHarness()
	v := h.create(t, KindVolume, "5", 400, time.Hour)
	s := h.create(t, KindServer, "5", 100, time.Hour)
	h.prov.volumes[400] = hcloud.Volume{ID: 400, Labels: v.Labels, Server: ptr(100)}
	h.clk.t = t0.Add(time.Hour)
	if err := h.col.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.prov.deleted, ",") != "server/100,volume/400" {
		t.Fatalf("deleted = %v", h.prov.deleted)
	}
	if h.state(t, s.ID) != StateDeleted || h.state(t, v.ID) != StateDeleted {
		t.Fatalf("states = %s %s", h.state(t, s.ID), h.state(t, v.ID))
	}
}

func TestCollectDeletesUnknownVolumesAndLooseAddresses(t *testing.T) {
	h := newHarness()
	keep := h.create(t, KindServer, "5", 100, time.Hour)
	h.prov.ips[500] = hcloud.PrimaryIP{ID: 500, AssigneeID: ptr(100)} // the live server's own address
	h.prov.ips[501] = hcloud.PrimaryIP{ID: 501}                       // made by hand, unassigned
	h.prov.volumes[401] = hcloud.Volume{ID: 401, Server: ptr(100)}    // made by hand, attached to a known server
	if err := h.col.Collect(context.Background()); err == nil {
		t.Fatal("want the attached unknown volume to wait for its detach")
	}
	if err := h.col.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.prov.deleted, ",") != "detach/401,primary_ip/501,volume/401" {
		t.Fatalf("deleted = %v", h.prov.deleted)
	}
	if h.state(t, keep.ID) != StateLive || len(h.prov.ips) != 1 {
		t.Fatalf("kept server %s, addresses %v", h.state(t, keep.ID), h.prov.ips)
	}
}

func TestReleaseServersFirstAndClosesNeverCreatedRows(t *testing.T) {
	h := newHarness()
	h.col.RetryWait = time.Millisecond
	f := h.create(t, KindFirewall, "5", 200, time.Hour)
	s := h.create(t, KindServer, "5", 100, time.Hour)
	v := h.create(t, KindVolume, "5", 400, time.Hour)
	h.prov.volumes[400] = hcloud.Volume{ID: 400, Labels: v.Labels, Server: ptr(100)}
	never, _ := h.reg.Insert(context.Background(), Resource{Project: "ci", Kind: KindServer, OwnerRef: "5", LeaseExpires: t0.Add(time.Hour)})
	byLabel, _ := h.reg.Insert(context.Background(), Resource{Project: "ci", Kind: KindFirewall, OwnerRef: "5", LeaseExpires: t0.Add(time.Hour)})
	h.prov.firewalls[201] = hcloud.Firewall{ID: 201, Labels: byLabel.Labels} // created, id never recorded
	rows, _ := h.reg.OpenByOwner(context.Background(), "ci", "5")
	if err := h.col.Release(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.prov.deleted, ",") != "server/100,volume/400,firewall/200,firewall/201" {
		t.Fatalf("deleted = %v", h.prov.deleted)
	}
	for _, id := range []int64{f.ID, s.ID, v.ID, never.ID, byLabel.ID} {
		if h.state(t, id) != StateDeleted {
			t.Fatalf("row %d = %s", id, h.state(t, id))
		}
	}
	// the label-found one recorded its id, so its hours are billed
	if got, _ := h.reg.Get(byLabel.ID); got.ProviderID != "201" {
		t.Fatalf("label-found row = %+v", got)
	}
}

// ruling 124: a 60-minute lease is deleted on the last pass before the hour
// ends (lease end within one interval plus the margin), never after it.
func TestCollectEndsALeaseOnTheLastPassBeforeItsHour(t *testing.T) {
	h := newHarness()
	h.col.Interval = 2 * time.Minute
	s := h.create(t, KindServer, "5", 100, time.Hour)
	h.clk.t = t0.Add(56*time.Minute + 29*time.Second) // ends in 3m31s, over 2m + 90s: one more pass fits
	if err := h.col.Collect(context.Background()); err != nil || len(h.prov.deleted) != 0 {
		t.Fatalf("deleted early: %v, %v", h.prov.deleted, err)
	}
	h.clk.t = t0.Add(56*time.Minute + 30*time.Second) // ends in 3m30s: the next pass would be late
	if err := h.col.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.prov.deleted, ",") != "server/100" || h.state(t, s.ID) != StateDeleted {
		t.Fatalf("deleted = %v, state %s", h.prov.deleted, h.state(t, s.ID))
	}
	if got, _ := h.reg.Get(s.ID); got.DeletedAt == nil || got.DeletedAt.Sub(got.CreatedAt) >= time.Hour {
		t.Fatalf("lived %v", got.DeletedAt.Sub(got.CreatedAt))
	}
}

// A create that landed after its row was closed as never created is
// deleted as unknown and its row adopts the provider id, so it is billed.
func TestCollectAdoptsALateCreate(t *testing.T) {
	h := newHarness()
	r, _ := h.reg.Insert(context.Background(), Resource{Project: "ci", Kind: KindServer, OwnerRef: "5", LeaseExpires: t0.Add(time.Hour)})
	h.reg.Transition(context.Background(), r.ID, StateDeleted)
	h.clk.t = t0.Add(5 * time.Minute)
	h.prov.servers[100] = hcloud.Server{ID: 100, Labels: r.Labels}
	if err := h.col.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := h.reg.Get(r.ID)
	if strings.Join(h.prov.deleted, ",") != "server/100" || got.ProviderID != "100" || got.State != StateDeleted || !got.DeletedAt.Equal(h.clk.t) {
		t.Fatalf("deleted %v, row %+v", h.prov.deleted, got)
	}
	// a foreign project's label is not adopted
	h.prov.servers[101] = hcloud.Server{ID: 101, Labels: map[string]string{"resource": itoa(r.ID)}}
	h.col.Collect(context.Background())
	if got, _ := h.reg.Get(r.ID); got.ProviderID != "100" {
		t.Fatalf("row = %+v", got)
	}
}

// ruling 124 with a volume: the lease's last pass deletes the whole
// commission, the volume waiting for its server's delete to detach it, and
// both deletes are issued before the lease ends. Modelled on the fake
// clock: every API call takes 1 s, a server delete completes 20 s after its
// call, retries wait 3 s. The detach from the deleting server is refused
// as locked, or with the 500 server_error Hetzner answered live.
func TestCollectEndsACommissionWithAVolumeBeforeItsHour(t *testing.T) {
	for _, refusal := range []*hcloud.APIError{nil, {Status: 500, Code: "server_error", Message: "internal server error"}} {
		testCommissionWithAVolumeEnds(t, refusal)
	}
}

func testCommissionWithAVolumeEnds(t *testing.T, refusal *hcloud.APIError) {
	ctx := context.Background()
	h := newHarness()
	h.prov.detachRefusal = refusal
	h.col.Interval = 2 * time.Minute
	h.col.RetryWait = 3 * time.Second
	h.col.After = func(d time.Duration) <-chan time.Time {
		h.clk.t = h.clk.t.Add(d)
		ch := make(chan time.Time, 1)
		ch <- h.clk.t
		return ch
	}
	lease := t0.Add(time.Hour)
	row := func(kind string, providerID int64) Resource {
		r, err := h.reg.Insert(ctx, Resource{Project: "ci", Kind: kind, OwnerRef: "5", LeaseExpires: lease, HoldID: 7})
		if err != nil {
			t.Fatal(err)
		}
		h.reg.SetLive(ctx, r.ID, itoa(providerID))
		return r
	}
	fw, srv, vol := row(KindFirewall, 200), row(KindServer, 100), row(KindVolume, 400)
	h.prov.firewalls[200] = hcloud.Firewall{ID: 200, Labels: fw.Labels}
	h.prov.servers[100] = hcloud.Server{ID: 100, Labels: srv.Labels}
	h.prov.volumes[400] = hcloud.Volume{ID: 400, Labels: vol.Labels, Server: ptr(100)}
	h.prov.clk, h.prov.callTook, h.prov.deleteTook = h.clk, time.Second, 20*time.Second

	// 56:25 + four listings = 56:29: the lease ends in 3m31s, one more pass fits
	h.clk.t = t0.Add(56*time.Minute + 25*time.Second)
	if err := h.col.Collect(ctx); err != nil || len(h.prov.deleted) != 0 {
		t.Fatalf("deleted early: %v, %v", h.prov.deleted, err)
	}
	// 58:25 + four listings = 58:29 (ends in 1m31s): server delete at 58:30,
	// done at 58:50; the volume is read and its detach refused (locked) at
	// 58:31/32, 58:36/37, 58:41/42, 58:46/47 with 3 s waits, read detached
	// at 58:51 and deleted at 58:52; the firewall at 58:53.
	h.clk.t = t0.Add(58*time.Minute + 25*time.Second)
	if err := h.col.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.prov.deleted, ",") != "server/100,volume/400,firewall/200" {
		t.Fatalf("deleted = %v", h.prov.deleted)
	}
	for name, want := range map[string]time.Duration{
		"server/100":   58*time.Minute + 30*time.Second,
		"volume/400":   58*time.Minute + 52*time.Second,
		"firewall/200": 58*time.Minute + 53*time.Second,
	} {
		if got := h.prov.calledAt[name]; !got.Equal(t0.Add(want)) || !got.Before(lease) {
			t.Fatalf("%s deleted at %v, want %v", name, got.Sub(t0), want)
		}
	}
	for _, r := range []Resource{fw, srv, vol} {
		if h.state(t, r.ID) != StateDeleted {
			t.Fatalf("row %d = %s", r.ID, h.state(t, r.ID))
		}
	}
}
