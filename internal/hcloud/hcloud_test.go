package hcloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{Token: "tok", BaseURL: srv.URL, HTTP: srv.Client()}
}

func TestCreateServerSendsRequestAndDecodes(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/servers" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &got); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"server":{"id":42,"name":"vm","status":"initializing","labels":{"factory":"ci"},
			"public_net":{"ipv4":{"id":7,"ip":"203.0.113.9"},"ipv6":null}},"action":{"id":1}}`)
	})
	s, err := c.CreateServer(context.Background(), CreateServerRequest{
		Name: "vm", ServerType: "cx23", Location: "fsn1", Image: "debian-12", UserData: "#!/bin/bash",
		Labels:    map[string]string{"factory": "ci"},
		Firewalls: []FirewallRef{{Firewall: 5}},
		PublicNet: &PublicNet{EnableIPv4: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != 42 || s.IPv4() != "203.0.113.9" || s.Labels["factory"] != "ci" {
		t.Fatalf("server = %+v", s)
	}
	want := map[string]any{
		"name": "vm", "server_type": "cx23", "location": "fsn1", "image": "debian-12", "user_data": "#!/bin/bash",
		"labels":     map[string]any{"factory": "ci"},
		"firewalls":  []any{map[string]any{"firewall": float64(5)}},
		"public_net": map[string]any{"enable_ipv4": true, "enable_ipv6": false},
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Fatalf("body = %s, want %s", gb, wb)
	}
}

func TestServersFollowsPagesAndSendsSelector(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if sel := r.URL.Query().Get("label_selector"); sel != "factory=ci" {
			t.Errorf("label_selector = %q", sel)
		}
		switch r.URL.Query().Get("page") {
		case "1":
			io.WriteString(w, `{"servers":[{"id":1},{"id":2}],"meta":{"pagination":{"page":1,"next_page":2}}}`)
		case "2":
			io.WriteString(w, `{"servers":[{"id":3}],"meta":{"pagination":{"page":2,"next_page":null}}}`)
		default:
			t.Errorf("page = %q", r.URL.Query().Get("page"))
		}
	})
	got, err := c.Servers(context.Background(), "factory=ci")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != 1 || got[2].ID != 3 {
		t.Fatalf("servers = %+v", got)
	}
}

func TestFirewallsListAllWithoutSelector(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/firewalls" || r.URL.Query().Has("label_selector") {
			t.Errorf("request = %s", r.URL)
		}
		io.WriteString(w, `{"firewalls":[{"id":9,"labels":{}}],"meta":{"pagination":{"next_page":null}}}`)
	})
	got, err := c.Firewalls(context.Background(), "")
	if err != nil || len(got) != 1 || got[0].ID != 9 {
		t.Fatalf("firewalls = %+v, %v", got, err)
	}
}

func TestDeleteIsIdempotentOn404(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s", r.Method)
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":"not_found","message":"gone"}}`)
	})
	if err := c.DeleteServer(context.Background(), 1); err != nil {
		t.Fatalf("delete server: %v", err)
	}
	if err := c.DeleteFirewall(context.Background(), 1); err != nil {
		t.Fatalf("delete firewall: %v", err)
	}
}

func TestErrorsCarryCodeAndNotFound(t *testing.T) {
	status := http.StatusConflict
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, `{"error":{"code":"resource_in_use","message":"firewall is applied"}}`)
	})
	err := c.DeleteFirewall(context.Background(), 1)
	if !IsCode(err, "resource_in_use") || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	status = http.StatusNotFound
	if _, err := c.Server(context.Background(), 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing server err = %v, want ErrNotFound", err)
	}
	if _, err := c.Firewall(context.Background(), 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing firewall err = %v, want ErrNotFound", err)
	}
}

func TestPricing(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"pricing":{"currency":"EUR",
			"primary_ips":[{"type":"ipv4","prices":[{"location":"fsn1","price_hourly":{"net":"0.0008000000","gross":"0.000952"}}]}],
			"server_types":[{"id":114,"name":"cx23","prices":[{"location":"fsn1","price_hourly":{"net":"0.0088000000","gross":"0.010472"}}]}]}}`)
	})
	p, err := c.Pricing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v, err := p.ServerHourlyNet("cx23", "fsn1"); err != nil || v != 0.0088 {
		t.Fatalf("cx23 = %v, %v", v, err)
	}
	if v, err := p.PrimaryIPHourlyNet("ipv4", "fsn1"); err != nil || v != 0.0008 {
		t.Fatalf("ipv4 = %v, %v", v, err)
	}
	if _, err := p.ServerHourlyNet("cx23", "nbg1"); err == nil {
		t.Fatal("want an error for a location without a price")
	}
	if _, err := p.ServerHourlyNet("cx99", "fsn1"); err == nil {
		t.Fatal("want an error for an unknown server type")
	}
}

func TestCreateVolumeAttachesAndDecodes(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/volumes" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"volume":{"id":77,"name":"v","size":20,"server":42,"labels":{"factory":"agent"}},"action":{"id":1}}`)
	})
	v, err := c.CreateVolume(context.Background(), CreateVolumeRequest{
		Name: "v", Size: 20, Server: 42, Automount: true, Format: "ext4", Labels: map[string]string{"factory": "agent"}})
	if err != nil {
		t.Fatal(err)
	}
	if v.ID != 77 || v.Server == nil || *v.Server != 42 || v.Size != 20 {
		t.Fatalf("volume = %+v", v)
	}
	gb, _ := json.Marshal(got)
	want := `{"automount":true,"format":"ext4","labels":{"factory":"agent"},"name":"v","server":42,"size":20}`
	if string(gb) != want {
		t.Fatalf("body = %s, want %s", gb, want)
	}
}

func TestVolumesAndPrimaryIPsListGetDeleteDetach(t *testing.T) {
	var calls []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.Method + " " + r.URL.Path {
		case "GET /volumes":
			io.WriteString(w, `{"volumes":[{"id":1,"server":null},{"id":2,"server":9}],"meta":{"pagination":{"next_page":null}}}`)
		case "GET /primary_ips":
			io.WriteString(w, `{"primary_ips":[{"id":5,"ip":"203.0.113.5","assignee_id":null},{"id":6,"ip":"203.0.113.6","assignee_id":9}],"meta":{"pagination":{"next_page":null}}}`)
		case "GET /volumes/1":
			io.WriteString(w, `{"volume":{"id":1}}`)
		case "GET /primary_ips/5":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":"not_found"}}`)
		case "POST /volumes/2/actions/detach":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"action":{"id":3}}`)
		case "DELETE /volumes/1", "DELETE /primary_ips/5":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":"not_found"}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	ctx := context.Background()
	vols, err := c.Volumes(ctx, "")
	if err != nil || len(vols) != 2 || vols[0].Server != nil || *vols[1].Server != 9 {
		t.Fatalf("volumes = %+v, %v", vols, err)
	}
	ips, err := c.PrimaryIPs(ctx, "")
	if err != nil || len(ips) != 2 || ips[0].AssigneeID != nil || *ips[1].AssigneeID != 9 || ips[0].IP != "203.0.113.5" {
		t.Fatalf("primary ips = %+v, %v", ips, err)
	}
	if _, err := c.Volume(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PrimaryIP(ctx, 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("primary ip 5 err = %v, want not found", err)
	}
	if err := c.DetachVolume(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteVolume(ctx, 1); err != nil {
		t.Fatalf("delete volume on 404: %v", err)
	}
	if err := c.DeletePrimaryIP(ctx, 5); err != nil {
		t.Fatalf("delete primary ip on 404: %v", err)
	}
}

func TestPricingMonthlyAndVolume(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"pricing":{"currency":"EUR","volume":{"price_per_gb_month":{"net":"0.0572000000","gross":"0.068068"}},
			"primary_ips":[{"type":"ipv4","prices":[{"location":"fsn1","price_hourly":{"net":"0.0008000000"},"price_monthly":{"net":"0.5000000000"}}]}],
			"server_types":[{"name":"cx23","prices":[{"location":"fsn1","price_hourly":{"net":"0.0088000000"},"price_monthly":{"net":"5.4900000000"}}]}]}}`)
	})
	p, err := c.Pricing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v, err := p.ServerMonthlyNet("cx23", "fsn1"); err != nil || v != 5.49 {
		t.Fatalf("cx23 monthly = %v, %v", v, err)
	}
	if v, err := p.PrimaryIPMonthlyNet("ipv4", "fsn1"); err != nil || v != 0.5 {
		t.Fatalf("ipv4 monthly = %v, %v", v, err)
	}
	if v, err := p.VolumeGBMonthNet(); err != nil || v != 0.0572 {
		t.Fatalf("volume = %v, %v", v, err)
	}
}
