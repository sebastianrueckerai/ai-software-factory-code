package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/hcloud"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

var cloudNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// fakeHetzner is an in-memory agent project. Every create checks that the
// registry already holds a requested row carrying the resource's labels.
type fakeHetzner struct {
	mu        sync.Mutex
	reg       *cloudres.Memory
	nextID    int64
	calls     []string
	servers   map[int64]hcloud.Server
	firewalls map[int64]hcloud.Firewall
	volumes   map[int64]hcloud.Volume
	serverReq hcloud.CreateServerRequest
	fwReq     hcloud.CreateFirewallRequest
	volReq    hcloud.CreateVolumeRequest
	serverErr error
	unordered []string // creates that found no requested row first
}

func newFakeHetzner(reg *cloudres.Memory) *fakeHetzner {
	return &fakeHetzner{reg: reg, nextID: 100, servers: map[int64]hcloud.Server{}, firewalls: map[int64]hcloud.Firewall{}, volumes: map[int64]hcloud.Volume{}}
}

func (f *fakeHetzner) rowFirst(kind string, labels map[string]string) {
	id, _ := strconv.ParseInt(labels["resource"], 10, 64)
	if r, ok := f.reg.Get(id); !ok || r.State != cloudres.StateRequested || r.Kind != kind {
		f.unordered = append(f.unordered, kind)
	}
}

func (f *fakeHetzner) Pricing(context.Context) (hcloud.Pricing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "pricing")
	var p hcloud.Pricing
	err := json.Unmarshal([]byte(`{"currency":"EUR","volume":{"price_per_gb_month":{"net":"0.0572000000"}},
		"primary_ips":[{"type":"ipv4","prices":[{"location":"fsn1","price_hourly":{"net":"0.0008000000"},"price_monthly":{"net":"0.5000000000"}}]}],
		"server_types":[{"name":"cx23","prices":[{"location":"fsn1","price_hourly":{"net":"0.0088000000"},"price_monthly":{"net":"5.4900000000"}}]},
		  {"name":"cx33","prices":[{"location":"fsn1","price_hourly":{"net":"0.0136000000"},"price_monthly":{"net":"8.4900000000"}}]}]}`), &p)
	return p, err
}

func (f *fakeHetzner) CreateFirewall(_ context.Context, r hcloud.CreateFirewallRequest) (hcloud.Firewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rowFirst(cloudres.KindFirewall, r.Labels)
	f.nextID++
	f.fwReq = r
	f.calls = append(f.calls, "create firewall")
	fw := hcloud.Firewall{ID: f.nextID, Name: r.Name, Labels: r.Labels}
	f.firewalls[fw.ID] = fw
	return fw, nil
}

func (f *fakeHetzner) CreateServer(_ context.Context, r hcloud.CreateServerRequest) (hcloud.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rowFirst(cloudres.KindServer, r.Labels)
	f.serverReq = r
	f.calls = append(f.calls, "create server")
	if f.serverErr != nil {
		return hcloud.Server{}, f.serverErr
	}
	f.nextID++
	var s hcloud.Server
	json.Unmarshal([]byte(`{"public_net":{"ipv4":{"ip":"203.0.113.`+strconv.FormatInt(f.nextID%250, 10)+`"}}}`), &s)
	s.ID, s.Name, s.Labels = f.nextID, r.Name, r.Labels
	f.servers[s.ID] = s
	return s, nil
}

func (f *fakeHetzner) CreateVolume(_ context.Context, r hcloud.CreateVolumeRequest) (hcloud.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rowFirst(cloudres.KindVolume, r.Labels)
	f.volReq = r
	f.calls = append(f.calls, "create volume")
	f.nextID++
	server := r.Server
	v := hcloud.Volume{ID: f.nextID, Name: r.Name, Size: r.Size, Server: &server, Labels: r.Labels}
	f.volumes[v.ID] = v
	return v, nil
}

func sortedValues[K cmp.Ordered, V any](m map[K]V) []V {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]V, 0, len(m))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

func (f *fakeHetzner) Servers(_ context.Context, sel string) ([]hcloud.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []hcloud.Server
	for _, s := range sortedValues(f.servers) {
		if k, v, _ := strings.Cut(sel, "="); sel == "" || s.Labels[k] == v {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeHetzner) Server(_ context.Context, id int64) (hcloud.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.servers[id]; ok {
		return s, nil
	}
	return hcloud.Server{}, &hcloud.APIError{Status: 404}
}

func (f *fakeHetzner) DeleteServer(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.servers, id)
	for vid, v := range f.volumes {
		if v.Server != nil && *v.Server == id {
			v.Server = nil
			f.volumes[vid] = v
		}
	}
	f.calls = append(f.calls, "delete server/"+strconv.FormatInt(id, 10))
	return nil
}

func (f *fakeHetzner) Firewalls(_ context.Context, sel string) ([]hcloud.Firewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []hcloud.Firewall
	for _, fw := range sortedValues(f.firewalls) {
		if k, v, _ := strings.Cut(sel, "="); sel == "" || fw.Labels[k] == v {
			out = append(out, fw)
		}
	}
	return out, nil
}

func (f *fakeHetzner) Firewall(_ context.Context, id int64) (hcloud.Firewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fw, ok := f.firewalls[id]; ok {
		return fw, nil
	}
	return hcloud.Firewall{}, &hcloud.APIError{Status: 404}
}

func (f *fakeHetzner) DeleteFirewall(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.firewalls, id)
	f.calls = append(f.calls, "delete firewall/"+strconv.FormatInt(id, 10))
	return nil
}

func (f *fakeHetzner) Volumes(_ context.Context, sel string) ([]hcloud.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []hcloud.Volume
	for _, v := range sortedValues(f.volumes) {
		if k, val, _ := strings.Cut(sel, "="); sel == "" || v.Labels[k] == val {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *fakeHetzner) Volume(_ context.Context, id int64) (hcloud.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.volumes[id]; ok {
		return v, nil
	}
	return hcloud.Volume{}, &hcloud.APIError{Status: 404}
}

func (f *fakeHetzner) DetachVolume(_ context.Context, id int64) error { return nil }

func (f *fakeHetzner) DeleteVolume(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.volumes, id)
	f.calls = append(f.calls, "delete volume/"+strconv.FormatInt(id, 10))
	return nil
}

func (f *fakeHetzner) PrimaryIPs(context.Context, string) ([]hcloud.PrimaryIP, error) {
	return nil, nil
}
func (f *fakeHetzner) DeletePrimaryIP(context.Context, int64) error { return nil }

func (f *fakeHetzner) callList() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(slices.DeleteFunc(slices.Clone(f.calls), func(c string) bool { return c == "pricing" }), ",")
}

// fakeCloudStore keeps the commissioning state over a Memory registry;
// Reserve reserves within the envelope unless outcome scripts it.
type fakeCloudStore struct {
	mu        sync.Mutex
	reg       *cloudres.Memory
	reqs      []cloudres.CommissionRequest
	outcome   *cloudres.Commission
	refusal   error
	ids       []int64
	addresses map[int64]string
	accrues   int
	overruns  []cloudres.Overrun
	approvals map[int64]cloudres.Approval
	hold      int64
}

func (f *fakeCloudStore) Use(context.Context, string) (cloudres.EnvelopeUse, error) {
	return cloudres.EnvelopeUse{OpenServers: 1, MonthEUR: 0.5}, nil
}

func (f *fakeCloudStore) Reserve(ctx context.Context, r cloudres.CommissionRequest) (cloudres.Commission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	if f.refusal != nil {
		return cloudres.Commission{}, f.refusal
	}
	if f.outcome != nil {
		return *f.outcome, nil
	}
	f.hold++
	c := cloudres.Commission{Status: cloudres.Reserved, Hold: policy.Hold{ID: f.hold, TaskID: r.TaskID, Kind: policy.KindResource, EUR: r.Quote.ReservationEUR}}
	kinds := []string{cloudres.KindFirewall, cloudres.KindServer}
	if r.VolumeGB > 0 {
		kinds = append(kinds, cloudres.KindVolume)
	}
	for _, kind := range kinds {
		row, _ := f.reg.Insert(ctx, cloudres.Resource{Project: cloudres.AgentProject, Kind: kind, OwnerRef: r.TaskID, Repo: r.Repo,
			Name: "agent-" + r.TaskID + "-" + kind, HoldID: f.hold, LeaseExpires: r.Now.Add(time.Duration(r.LeaseMinutes) * time.Minute),
			Labels: map[string]string{"task": cloudres.LabelValue(r.TaskID), "project": cloudres.LabelValue(r.Repo)}})
		f.ids = append(f.ids, row.ID)
		c.Rows = append(c.Rows, row)
	}
	return c, nil
}

func (f *fakeCloudStore) SetAddress(_ context.Context, id int64, address string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addresses[id] = address
	return nil
}

func (f *fakeCloudStore) rows(keep func(cloudres.Resource) bool) []cloudres.Resource {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []cloudres.Resource
	for _, id := range f.ids {
		r, _ := f.reg.Get(id)
		r.Address = f.addresses[id]
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeCloudStore) Owned(_ context.Context, taskID string) ([]cloudres.Resource, error) {
	return f.rows(func(r cloudres.Resource) bool { return r.OwnerRef == taskID }), nil
}

func (f *fakeCloudStore) Group(_ context.Context, hold int64) ([]cloudres.Resource, error) {
	return f.rows(func(r cloudres.Resource) bool {
		return r.HoldID == hold && slices.Contains(cloudres.OpenStates, r.State)
	}), nil
}

func (f *fakeCloudStore) Approval(_ context.Context, id int64, taskID string) (cloudres.Approval, bool, error) {
	a, ok := f.approvals[id]
	return a, ok && taskID == "42", nil
}

func (f *fakeCloudStore) Accrue(context.Context) (cloudres.Accrual, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accrues++
	return cloudres.Accrual{Overruns: f.overruns}, nil
}

func (f *fakeCloudStore) Session(context.Context, string) (string, error) { return "s-1", nil }

type fakeSigner struct {
	mu    sync.Mutex
	signs []string
}

func (f *fakeSigner) CAPublicKey(context.Context) (string, error) {
	return "ssh-ed25519 AAAACAKEY factory-ca", nil
}

func (f *fakeSigner) Sign(_ context.Context, key, principal string, ttl time.Duration, keyID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signs = append(f.signs, fmt.Sprintf("%s|%s|%s|%s", key, principal, ttl, keyID))
	return "ssh-ed25519-cert-v01@openssh.com AAAACERT", nil
}

type cloudEnv struct {
	*toolEnv
	hz     *fakeHetzner
	cs     *fakeCloudStore
	signer *fakeSigner
	reg    *cloudres.Memory
	cloud  *Cloud
}

func coderTask() Task {
	t := running("coder")
	t.Repository = "sf/app"
	return t
}

func cloudPolicy() policy.Version {
	v := toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2})
	v.Document.EnvelopeDefault = policy.Envelope{MonthlyEUR: 10, ServerTypes: []string{"cx23", "cx33"}, MaxCount: 2, MaxLeaseHours: 4, VolumeMaxGB: 20}
	return v
}

// newCloudEnv serves task under cloudPolicy, or under pv when given.
func newCloudEnv(t *testing.T, task Task, pv ...policy.Version) *cloudEnv {
	t.Helper()
	reg := cloudres.NewMemory()
	reg.Now = func() time.Time { return cloudNow }
	hz := newFakeHetzner(reg)
	cs := &fakeCloudStore{reg: reg, addresses: map[int64]string{}, approvals: map[int64]cloudres.Approval{}}
	signer := &fakeSigner{}
	store := newMemStore(task)
	store.setPolicy(append(pv, cloudPolicy())[0])
	cloud := &Cloud{Provider: hz, Store: cs, Registry: reg, Signer: signer, Sources: []string{"198.51.100.1/32", "198.51.100.2/32"},
		Collector: &cloudres.Collector{Project: cloudres.AgentProject, Registry: reg, Provider: hz, RetryWait: time.Nanosecond, NodeIPs: []string{"198.51.100.1/32"}},
		Audit:     store, Now: func() time.Time { return cloudNow }}
	s := NewServer(ServerOptions{Auth: fakeAuth{id: Identity{TaskID: task.ID, SAUID: "u"}}, Store: store, Minter: &fakeMinter{},
		Prices: testPrices, Escalator: &fakeEscalator{}, Cloud: cloud})
	srv := httptest.NewServer(s.Routes("test"))
	t.Cleanup(srv.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return &cloudEnv{toolEnv: &toolEnv{store: store, session: session}, hz: hz, cs: cs, signer: signer, reg: reg, cloud: cloud}
}

const agentKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGJ0ZXN0a2V5 agent@sandbox"

func TestCloudToolsListed(t *testing.T) {
	e := newCloudEnv(t, coderTask())
	res, err := e.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	want := "cloud_prices,commission_server,delegate,escalate_to_human,fal_run,file_plan,file_verdict,list_resources,manual,release_resource,replicate_predict,request_info,resource_request_status,ssh_certificate,web_scrape,web_search"
	if strings.Join(names, ",") != want {
		t.Fatalf("tools = %v", names)
	}
}

func TestCommissionRegistersBeforeEachCreate(t *testing.T) {
	e := newCloudEnv(t, coderTask())
	text, isErr := e.call(t, policy.ToolCommission, map[string]any{"server_type": "cx23", "purpose": "load test", "lease_minutes": 90,
		"volume_gb": 20, "ssh_public_key": agentKey})
	if isErr {
		t.Fatal(text)
	}
	if got := e.hz.callList(); got != "create firewall,create server,create volume" {
		t.Fatalf("provider calls = %s", got)
	}
	if len(e.hz.unordered) != 0 {
		t.Fatalf("created without a requested row first: %v", e.hz.unordered)
	}
	// the request as priced: 2 started hours of cx23 + IPv4 + 20 GB
	r := e.cs.reqs[0]
	if r.TaskID != "42" || r.Repo != "sf/app" || r.Session != "s-1" || r.Location != "fsn1" || r.LeaseMinutes != 90 || r.VolumeGB != 20 ||
		r.Quote.Hours != 2 || !near6(r.Quote.ReservationEUR, 0.0192+2*1.144/730) || r.Envelope.MaxCount != 2 || r.DayEUR != 20 || r.MonthEUR != 50 ||
		r.Project != "" || !math.IsInf(r.ProjectEUR, 1) {
		t.Fatalf("reserve request = %+v", r)
	}
	sr := e.hz.serverReq
	if sr.ServerType != "cx23" || sr.Location != "fsn1" || sr.Image != "debian-12" || len(sr.Firewalls) != 1 || sr.Firewalls[0].Firewall != 101 ||
		sr.PublicNet == nil || !sr.PublicNet.EnableIPv4 || sr.PublicNet.EnableIPv6 {
		t.Fatalf("server request = %+v", sr)
	}
	for k, v := range map[string]string{"factory": "agent", "task": "42", "project": "sf--app", "resource": "2"} {
		if sr.Labels[k] != v {
			t.Fatalf("server label %s = %q, want %q (labels %v)", k, sr.Labels[k], v, sr.Labels)
		}
	}
	for _, want := range []string{"ssh-ed25519 AAAACAKEY factory-ca", "TrustedUserCAKeys /etc/ssh/factory-user-ca.pub", "AuthorizedKeysFile none", "PasswordAuthentication no",
		"- path: /etc/ssh/factory-principals/root\n  permissions: '0644'\n  content: |\n    resource-2\n",
		"AuthorizedPrincipalsFile /etc/ssh/factory-principals/%u",
		"- [usermod, -p, '*', root]", "- [chage, -d, '-1', root]"} {
		if !strings.Contains(sr.UserData, want) {
			t.Fatalf("user data lacks %q:\n%s", want, sr.UserData)
		}
	}
	fr := e.hz.fwReq.Rules
	if len(fr) == 0 || fr[0].Direction != "in" || fr[0].Port != "22" || strings.Join(fr[0].SourceIPs, ",") != "198.51.100.1/32,198.51.100.2/32" {
		t.Fatalf("firewall rules = %+v", fr)
	}
	for _, rule := range fr[1:] {
		if rule.Direction != "out" {
			t.Fatalf("a second inbound rule: %+v", rule)
		}
	}
	if vr := e.hz.volReq; vr.Server != 102 || vr.Size != 20 || !vr.Automount || vr.Format != "ext4" || vr.Labels["resource"] != "3" {
		t.Fatalf("volume request = %+v", vr)
	}
	for id, provider := range map[int64]string{1: "101", 2: "102", 3: "103"} {
		if row, _ := e.reg.Get(id); row.State != cloudres.StateLive || row.ProviderID != provider {
			t.Fatalf("row %d = %+v", id, row)
		}
	}
	if e.cs.addresses[2] != "203.0.113.102" {
		t.Fatalf("address = %v", e.cs.addresses)
	}
	for _, want := range []string{"commissioned resource 2", "203.0.113.102", "lease until 2026-09-28T13:30:00Z", "Reserved 0.022334",
		"/mnt/HC_Volume_103", "ssh-ed25519-cert-v01@openssh.com AAAACERT"} {
		if !strings.Contains(text, want) {
			t.Fatalf("answer lacks %q:\n%s", want, text)
		}
	}
	if len(e.signer.signs) != 1 || e.signer.signs[0] != agentKey+"|resource-2|30m0s|task=42 resource=2" {
		t.Fatalf("signs = %v", e.signer.signs)
	}
	a := e.lastAudit(t)
	if a.Tool != policy.ToolCommission || a.Decision != "allowed" || a.HoldEUR == nil || !near6(*a.HoldEUR, 0.0192+2*1.144/730) {
		t.Fatalf("audit = %+v", a)
	}
}

func TestCommissionBeyondEnvelopeIsPending(t *testing.T) {
	e := newCloudEnv(t, coderTask())
	e.cs.outcome = &cloudres.Commission{Status: cloudres.Pending, ApprovalID: 7, Violations: []string{"a lease of 300 minutes exceeds the envelope's 4 h"}}
	text, isErr := e.call(t, policy.ToolCommission, map[string]any{"server_type": "cx33", "purpose": "soak", "lease_minutes": 300})
	if isErr || !strings.HasPrefix(text, "pending human approval 7") || !strings.Contains(text, "resource_request_status") {
		t.Fatalf("answer = %v %s", isErr, text)
	}
	if got := e.hz.callList(); got != "" {
		t.Fatalf("provider calls = %s", got)
	}
	if a := e.lastAudit(t); a.Decision != "pending_approval" || !strings.Contains(a.Reason, "approval request 7") {
		t.Fatalf("audit = %+v", a)
	}
	e.cs.approvals[7] = cloudres.Approval{ID: 7, Status: "approved"}
	text, isErr = e.call(t, policy.ToolRequestStatus, map[string]any{"request_id": 7})
	if isErr || !strings.Contains(text, "approved: call commission_server again") {
		t.Fatalf("status = %s", text)
	}
	if text, isErr := e.call(t, policy.ToolRequestStatus, map[string]any{"request_id": 8}); !isErr || !strings.Contains(text, "refused_arguments") {
		t.Fatalf("unknown request = %s", text)
	}

	e.cs.outcome = &cloudres.Commission{Status: cloudres.Denied, ApprovalID: 7, Note: "too long"}
	text, isErr = e.call(t, policy.ToolCommission, map[string]any{"server_type": "cx33", "purpose": "soak", "lease_minutes": 300})
	if !isErr || !strings.Contains(text, "a human denied this request (approval request 7): too long") {
		t.Fatalf("denied = %s", text)
	}
}

func TestCommissionRefusals(t *testing.T) {
	e := newCloudEnv(t, coderTask())
	for name, args := range map[string]map[string]any{
		"type":    {"server_type": "CX23;", "purpose": "p", "lease_minutes": 60},
		"purpose": {"server_type": "cx23", "purpose": " ", "lease_minutes": 60},
		"lease":   {"server_type": "cx23", "purpose": "p", "lease_minutes": 0},
		"short":   {"server_type": "cx23", "purpose": "p", "lease_minutes": 4},
		"volume":  {"server_type": "cx23", "purpose": "p", "lease_minutes": 60, "volume_gb": 5},
		"key":     {"server_type": "cx23", "purpose": "p", "lease_minutes": 60, "ssh_public_key": "ssh-ed25519 AAAA\nrm -rf /"},
		"price":   {"server_type": "cx99", "purpose": "p", "lease_minutes": 60},
		// ruling 147: standing resources come from registration or a human
		"standing": {"server_type": "cx23", "purpose": "p", "lease_minutes": 60, "lifetime": "standing"},
		"lifetime": {"server_type": "cx23", "purpose": "p", "lease_minutes": 60, "lifetime": "forever"},
	} {
		text, isErr := e.call(t, policy.ToolCommission, args)
		if !isErr || !strings.Contains(text, "refused by the factory policy") {
			t.Fatalf("%s: %v %s", name, isErr, text)
		}
	}
	if len(e.cs.reqs) != 0 || e.hz.callList() != "" {
		t.Fatalf("refused calls reached the store or provider: %d, %s", len(e.cs.reqs), e.hz.callList())
	}
	e.cs.refusal = &policy.Refusal{Decision: "refused_budget", Reason: "cx23: worst case 0.0096 EUR exceeds the free headroom"}
	if text, isErr := e.call(t, policy.ToolCommission, map[string]any{"server_type": "cx23", "purpose": "p", "lease_minutes": 60}); !isErr ||
		!strings.Contains(text, "(refused_budget)") {
		t.Fatalf("budget = %s", text)
	}

	for _, task := range []Task{{ID: "42", Status: "done", Classes: []string{"coder"}, Repository: "sf/app"}, {ID: "42", Status: "running", Classes: []string{"other"}, Repository: "sf/app"}} {
		e := newCloudEnv(t, task)
		if text, isErr := e.call(t, policy.ToolCommission, map[string]any{"server_type": "cx23", "purpose": "p", "lease_minutes": 60}); !isErr ||
			!strings.Contains(text, "refused") {
			t.Fatalf("task %+v: %s", task, text)
		}
	}
}

func TestCommissionCreateFailureReleasesEverything(t *testing.T) {
	e := newCloudEnv(t, coderTask())
	e.hz.serverErr = &hcloud.APIError{Status: 422, Code: "invalid_input", Message: "no"}
	text, isErr := e.call(t, policy.ToolCommission, map[string]any{"server_type": "cx23", "purpose": "p", "lease_minutes": 60, "volume_gb": 10})
	if !isErr || !strings.Contains(text, "commissioning failed: create server") {
		t.Fatalf("answer = %s", text)
	}
	if got := e.hz.callList(); got != "create firewall,create server,delete firewall/101" {
		t.Fatalf("provider calls = %s", got)
	}
	for id := int64(1); id <= 3; id++ {
		if row, _ := e.reg.Get(id); row.State != cloudres.StateDeleted {
			t.Fatalf("row %d = %s", id, row.State)
		}
	}
	if e.cs.accrues != 1 {
		t.Fatalf("accrual passes = %d", e.cs.accrues)
	}
}

func TestReleaseAndCertificates(t *testing.T) {
	e := newCloudEnv(t, coderTask())
	if text, isErr := e.call(t, policy.ToolCommission, map[string]any{"server_type": "cx23", "purpose": "p", "lease_minutes": 10, "volume_gb": 10}); isErr {
		t.Fatal(text)
	}
	// a certificate never outlives the egress: 10 minutes left, the address
	// leaves 2 min + 90 s before the end: 6m30s
	text, isErr := e.call(t, policy.ToolSSHCertificate, map[string]any{"resource_id": 2, "public_key": agentKey})
	if isErr || !strings.Contains(text, "root@203.0.113.102, valid 6m30s") || e.signer.signs[0] != agentKey+"|resource-2|6m30s|task=42 resource=2" {
		t.Fatalf("certificate = %s, signs %v", text, e.signer.signs)
	}
	for name, args := range map[string]map[string]any{
		"firewall": {"resource_id": 1, "public_key": agentKey},
		"unknown":  {"resource_id": 99, "public_key": agentKey},
		"bad key":  {"resource_id": 2, "public_key": "not a key"},
	} {
		if text, isErr := e.call(t, policy.ToolSSHCertificate, args); !isErr || !strings.Contains(text, "refused_arguments") {
			t.Fatalf("%s: %s", name, text)
		}
	}
	text, _ = e.call(t, policy.ToolListResources, map[string]any{})
	if !strings.Contains(text, "resource 2: server") || !strings.Contains(text, "203.0.113.102") || !strings.Contains(text, "resource 3: volume") ||
		strings.Contains(text, "firewall") {
		t.Fatalf("list = %s", text)
	}
	text, isErr = e.call(t, policy.ToolRelease, map[string]any{"resource_id": 2})
	if isErr || !strings.Contains(text, "resource 2 released") {
		t.Fatalf("release = %s", text)
	}
	if got := e.hz.callList(); got != "create firewall,create server,create volume,delete server/102,delete volume/103,delete firewall/101" {
		t.Fatalf("provider calls = %s", got)
	}
	for id := int64(1); id <= 3; id++ {
		if row, _ := e.reg.Get(id); row.State != cloudres.StateDeleted {
			t.Fatalf("row %d = %s", id, row.State)
		}
	}
	if e.cs.accrues != 1 {
		t.Fatalf("accrual passes = %d", e.cs.accrues)
	}
	if text, _ := e.call(t, policy.ToolRelease, map[string]any{"resource_id": 2}); !strings.Contains(text, "already released") {
		t.Fatalf("second release = %s", text)
	}
	if text, isErr := e.call(t, policy.ToolSSHCertificate, map[string]any{"resource_id": 2, "public_key": agentKey}); !isErr || !strings.Contains(text, "is deleted") {
		t.Fatalf("certificate for a deleted server = %s", text)
	}
}

func TestCloudPrices(t *testing.T) {
	e := newCloudEnv(t, coderTask())
	text, isErr := e.call(t, policy.ToolCloudPrices, map[string]any{})
	for _, want := range []string{"- cx23: 0.0096 EUR/h, 5.99 EUR/month", "- cx33: 0.0144 EUR/h, 8.99 EUR/month", "0.0572 EUR per GB",
		"the standard envelope (policy version 7)", "10.00 EUR a month (0.500000 used", "at most 2 servers at once (1 open), leases up to 4 h"} {
		if isErr || !strings.Contains(text, want) {
			t.Fatalf("prices lack %q:\n%s", want, text)
		}
	}
	// the catalogue is read once an hour
	if n := strings.Count(strings.Join(e.hz.calls, ","), "pricing"); n != 1 {
		t.Fatalf("pricing fetched %d times", n)
	}
	// ruling 183: the envelope of the project's file
	v := cloudPolicy()
	v.Projects = map[string]policy.ProjectConfig{"app": {Repository: "sf/app", MergeRule: "human", Objects: "factory", Agents: "coder",
		Envelope: &policy.Envelope{MonthlyEUR: 3, ServerTypes: []string{"cx23"}, MaxCount: 1, MaxLeaseHours: 1}, Budget: &policy.ProjectBudget{MonthEUR: 7}}}
	e = newCloudEnv(t, coderTask(), v)
	text, _ = e.call(t, policy.ToolCloudPrices, map[string]any{})
	if !strings.Contains(text, "project app's envelope (projects/app.yaml, policy version 7)") || strings.Contains(text, "cx33") ||
		!strings.Contains(text, "3.00 EUR a month") {
		t.Fatalf("project envelope prices = %s", text)
	}
	if _, isErr := e.call(t, policy.ToolCommission, map[string]any{"server_type": "cx23", "purpose": "load test", "lease_minutes": 60}); isErr ||
		e.cs.reqs[0].Envelope.MonthlyEUR != 3 || e.cs.reqs[0].Project != "app" || e.cs.reqs[0].ProjectEUR != 7 {
		t.Fatalf("the reservation's envelope and project cap = %+v", e.cs.reqs[0])
	}
}

func TestTaskOwnerDone(t *testing.T) {
	store := newMemStore(Task{ID: "run", Status: "running"}, Task{ID: "ok", Status: "done"}, Task{ID: "bad", Status: "failed"})
	done := TaskOwnerDone(store)
	for id, want := range map[string]bool{"run": false, "ok": true, "bad": true, "gone": true} {
		if got, err := done(context.Background(), id); err != nil || got != want {
			t.Fatalf("%s = %v, %v", id, got, err)
		}
	}
}

func TestCommissionUserDataRefusesAMultiLineKey(t *testing.T) {
	if _, err := commissionUserData("ssh-rsa AAAA\nruncmd: evil", "resource-1"); err == nil {
		t.Fatal("want an error")
	}
	if _, err := commissionUserData("x", "resource-1"); err != nil {
		t.Fatal(err)
	}
}

// ruling 125: a certificate is only ever signed for the task's own server,
// with that server's principal.
func TestCertificateForAnotherTasksServerIsRefused(t *testing.T) {
	e := newCloudEnv(t, coderTask())
	other, _ := e.reg.Insert(context.Background(), cloudres.Resource{Project: cloudres.AgentProject, Kind: cloudres.KindServer,
		OwnerRef: "other-task", HoldID: 9, LeaseExpires: cloudNow.Add(time.Hour)})
	e.reg.SetLive(context.Background(), other.ID, "555")
	e.cs.ids = append(e.cs.ids, other.ID)
	text, isErr := e.call(t, policy.ToolSSHCertificate, map[string]any{"resource_id": other.ID, "public_key": agentKey})
	if !isErr || !strings.Contains(text, "not one of this task's servers") || len(e.signer.signs) != 0 {
		t.Fatalf("certificate for another task's server = %s, signs %v", text, e.signer.signs)
	}
}

// ruling 124: an accrual past its reservation is booked in full and
// flagged by the store, and audited here.
func TestAccrualOverrunIsAudited(t *testing.T) {
	e := newCloudEnv(t, coderTask())
	e.cs.overruns = []cloudres.Overrun{{TaskID: "42", ResourceID: 38, EUR: 0.0192, HeldEUR: 0.0096, Reason: "COST OVER HOLD: resource 38"}}
	if err := e.cloud.accrue(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := e.lastAudit(t)
	if a.Decision != "accrued" || a.TaskID != "42" || a.Tool != policy.ToolCommission || *a.SpendEUR != 0.0192 || *a.HoldEUR != 0.0096 ||
		a.OverHold != "COST OVER HOLD: resource 38" {
		t.Fatalf("audit = %+v", a)
	}
}

// Ruling 132: the free tools follow the policy's class lists like the paid
// ones, no class name in code: a class that lists escalate_to_human may
// escalate, a coder class that does not list commission_server may not
// commission.
func TestFreeToolsFollowThePolicysClassLists(t *testing.T) {
	v := cloudPolicy()
	v.Document.Classes["odd"] = policy.Class{Models: []string{"unpriced"}, Tools: map[string]policy.ToolCap{"escalate_to_human": {}}}
	delete(v.Document.Classes["coder"].Tools, "commission_server")
	odd := newToolEnv(t, running("odd"), v)
	if text, isErr := odd.call(t, "escalate_to_human", map[string]any{"question": "q", "reason": "r", "recommendation": "x"}); isErr {
		t.Fatalf("escalate for a class that lists it = %s", text)
	}
	e := newCloudEnv(t, coderTask(), v)
	text, isErr := e.call(t, "commission_server", map[string]any{"server_type": "cx23", "purpose": "load test", "lease_minutes": 90, "ssh_public_key": agentKey})
	if !isErr || !strings.Contains(text, "refused_capability") || e.hz.callList() != "" {
		t.Fatalf("commission for a class that does not list it = %s, calls %q", text, e.hz.callList())
	}
}

// ruling 147: agents get no SSH to a standing server, even their task's own.
func TestCertificateForStandingServerIsRefused(t *testing.T) {
	e := newCloudEnv(t, coderTask())
	own, _ := e.reg.Insert(context.Background(), cloudres.Resource{Project: cloudres.AgentProject, Kind: cloudres.KindServer,
		OwnerRef: "42", HoldID: 9, LeaseExpires: cloudNow.Add(time.Hour), Lifetime: cloudres.LifetimeStanding})
	e.reg.SetLive(context.Background(), own.ID, "556")
	e.cs.ids = append(e.cs.ids, own.ID)
	text, isErr := e.call(t, policy.ToolSSHCertificate, map[string]any{"resource_id": own.ID, "public_key": agentKey})
	if !isErr || !strings.Contains(text, "standing") || len(e.signer.signs) != 0 {
		t.Fatalf("certificate for a standing server = %s, signs %v", text, e.signer.signs)
	}
}

// ruling 147: a standing server lives until a human releases it; the task
// that commissioned it cannot.
func TestReleaseOfStandingServerIsRefused(t *testing.T) {
	e := newCloudEnv(t, coderTask())
	own, _ := e.reg.Insert(context.Background(), cloudres.Resource{Project: cloudres.AgentProject, Kind: cloudres.KindServer,
		OwnerRef: "42", HoldID: 9, LeaseExpires: cloudNow.Add(time.Hour), Lifetime: cloudres.LifetimeStanding})
	e.reg.SetLive(context.Background(), own.ID, "557")
	e.cs.ids = append(e.cs.ids, own.ID)
	text, isErr := e.call(t, policy.ToolRelease, map[string]any{"resource_id": own.ID})
	if !isErr || !strings.Contains(text, "only a human on the console releases it") || e.hz.callList() != "" {
		t.Fatalf("release of a standing server = %s, provider calls %q", text, e.hz.callList())
	}
	if row, _ := e.reg.Get(own.ID); row.State != cloudres.StateLive {
		t.Fatalf("state = %s", row.State)
	}
}
