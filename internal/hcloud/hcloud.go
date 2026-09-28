// Package hcloud is a thin Hetzner Cloud API client: servers, firewalls,
// volumes, primary IPs and pricing, bearer-token auth. It is project-agnostic: the token decides the
// project.
package hcloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const DefaultBaseURL = "https://api.hetzner.cloud/v1"

// ErrNotFound matches an API 404 (errors.Is).
var ErrNotFound = errors.New("hcloud: not found")

// APIError is a non-2xx answer; Code is Hetzner's error code, e.g.
// resource_limit_exceeded or resource_in_use.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("hcloud: %d %s: %s", e.Status, e.Code, e.Message)
}

func (e *APIError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

// IsCode reports whether err is an APIError with the given code.
func IsCode(err error, code string) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == code
}

type Client struct {
	Token   string
	BaseURL string // DefaultBaseURL when empty
	HTTP    *http.Client
}

func New(token string) *Client {
	return &Client{Token: token, BaseURL: DefaultBaseURL, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e) // #nosec G104 -- a non-JSON error body still yields the status
		return &APIError{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

type pagination struct {
	Meta struct {
		Pagination struct {
			NextPage *int `json:"next_page"`
		} `json:"pagination"`
	} `json:"meta"`
}

// list walks every page of a collection; decode appends one page's items.
func (c *Client) list(ctx context.Context, path, labelSelector string, decode func([]byte) error) error {
	for page := 1; ; {
		q := url.Values{"page": {strconv.Itoa(page)}, "per_page": {"50"}}
		if labelSelector != "" {
			q.Set("label_selector", labelSelector)
		}
		var raw json.RawMessage
		if err := c.do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, &raw); err != nil {
			return err
		}
		if err := decode(raw); err != nil {
			return err
		}
		var p pagination
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		if p.Meta.Pagination.NextPage == nil {
			return nil
		}
		page = *p.Meta.Pagination.NextPage
	}
}

type Server struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Labels    map[string]string `json:"labels"`
	Created   time.Time         `json:"created"`
	PublicNet struct {
		IPv4 *struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
}

// IPv4 is the server's public IPv4, "" without one.
func (s Server) IPv4() string {
	if s.PublicNet.IPv4 == nil {
		return ""
	}
	return s.PublicNet.IPv4.IP
}

type CreateServerRequest struct {
	Name       string            `json:"name"`
	ServerType string            `json:"server_type"`
	Location   string            `json:"location"`
	Image      string            `json:"image"`
	UserData   string            `json:"user_data,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Firewalls  []FirewallRef     `json:"firewalls,omitempty"`
	PublicNet  *PublicNet        `json:"public_net,omitempty"`
}

type FirewallRef struct {
	Firewall int64 `json:"firewall"`
}

type PublicNet struct {
	EnableIPv4 bool `json:"enable_ipv4"`
	EnableIPv6 bool `json:"enable_ipv6"`
}

func (c *Client) CreateServer(ctx context.Context, r CreateServerRequest) (Server, error) {
	var out struct {
		Server Server `json:"server"`
	}
	err := c.do(ctx, http.MethodPost, "/servers", r, &out)
	return out.Server, err
}

func (c *Client) Server(ctx context.Context, id int64) (Server, error) {
	var out struct {
		Server Server `json:"server"`
	}
	err := c.do(ctx, http.MethodGet, "/servers/"+strconv.FormatInt(id, 10), nil, &out)
	return out.Server, err
}

// Servers lists the project's servers matching labelSelector ("" = all).
func (c *Client) Servers(ctx context.Context, labelSelector string) ([]Server, error) {
	var all []Server
	err := c.list(ctx, "/servers", labelSelector, func(raw []byte) error {
		var page struct {
			Servers []Server `json:"servers"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		all = append(all, page.Servers...)
		return nil
	})
	return all, err
}

// DeleteServer is idempotent: a server already gone is success.
func (c *Client) DeleteServer(ctx context.Context, id int64) error {
	return ignoreNotFound(c.do(ctx, http.MethodDelete, "/servers/"+strconv.FormatInt(id, 10), nil, nil))
}

type Firewall struct {
	ID      int64             `json:"id"`
	Name    string            `json:"name"`
	Labels  map[string]string `json:"labels"`
	Created time.Time         `json:"created"`
	Rules   []FirewallRule    `json:"rules"`
}

type FirewallRule struct {
	Direction      string   `json:"direction"` // in | out
	Protocol       string   `json:"protocol"`  // tcp | udp | icmp | esp | gre
	Port           string   `json:"port,omitempty"`
	SourceIPs      []string `json:"source_ips,omitempty"`
	DestinationIPs []string `json:"destination_ips,omitempty"`
}

type CreateFirewallRequest struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Rules  []FirewallRule    `json:"rules"`
}

func (c *Client) CreateFirewall(ctx context.Context, r CreateFirewallRequest) (Firewall, error) {
	var out struct {
		Firewall Firewall `json:"firewall"`
	}
	err := c.do(ctx, http.MethodPost, "/firewalls", r, &out)
	return out.Firewall, err
}

func (c *Client) Firewall(ctx context.Context, id int64) (Firewall, error) {
	var out struct {
		Firewall Firewall `json:"firewall"`
	}
	err := c.do(ctx, http.MethodGet, "/firewalls/"+strconv.FormatInt(id, 10), nil, &out)
	return out.Firewall, err
}

// Firewalls lists the project's firewalls matching labelSelector ("" = all).
func (c *Client) Firewalls(ctx context.Context, labelSelector string) ([]Firewall, error) {
	var all []Firewall
	err := c.list(ctx, "/firewalls", labelSelector, func(raw []byte) error {
		var page struct {
			Firewalls []Firewall `json:"firewalls"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		all = append(all, page.Firewalls...)
		return nil
	})
	return all, err
}

// DeleteFirewall is idempotent: a firewall already gone is success. A
// firewall still applied to a server that is being deleted answers
// resource_in_use until the server is gone.
func (c *Client) DeleteFirewall(ctx context.Context, id int64) error {
	return ignoreNotFound(c.do(ctx, http.MethodDelete, "/firewalls/"+strconv.FormatInt(id, 10), nil, nil))
}

func ignoreNotFound(err error) error {
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

type Volume struct {
	ID      int64             `json:"id"`
	Name    string            `json:"name"`
	Size    int               `json:"size"`
	Server  *int64            `json:"server"` // nil when detached
	Labels  map[string]string `json:"labels"`
	Created time.Time         `json:"created"`
}

// CreateVolumeRequest attaches the volume to Server (which fixes its
// location) and, with Automount and Format, mounts it there.
type CreateVolumeRequest struct {
	Name      string            `json:"name"`
	Size      int               `json:"size"`
	Server    int64             `json:"server"`
	Automount bool              `json:"automount"`
	Format    string            `json:"format,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

func (c *Client) CreateVolume(ctx context.Context, r CreateVolumeRequest) (Volume, error) {
	var out struct {
		Volume Volume `json:"volume"`
	}
	err := c.do(ctx, http.MethodPost, "/volumes", r, &out)
	return out.Volume, err
}

func (c *Client) Volume(ctx context.Context, id int64) (Volume, error) {
	var out struct {
		Volume Volume `json:"volume"`
	}
	err := c.do(ctx, http.MethodGet, "/volumes/"+strconv.FormatInt(id, 10), nil, &out)
	return out.Volume, err
}

// Volumes lists the project's volumes matching labelSelector ("" = all).
func (c *Client) Volumes(ctx context.Context, labelSelector string) ([]Volume, error) {
	var all []Volume
	err := c.list(ctx, "/volumes", labelSelector, func(raw []byte) error {
		var page struct {
			Volumes []Volume `json:"volumes"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		all = append(all, page.Volumes...)
		return nil
	})
	return all, err
}

// DetachVolume starts detaching a volume; it cannot be deleted while
// attached.
func (c *Client) DetachVolume(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodPost, "/volumes/"+strconv.FormatInt(id, 10)+"/actions/detach", nil, nil)
}

// DeleteVolume is idempotent: a volume already gone is success.
func (c *Client) DeleteVolume(ctx context.Context, id int64) error {
	return ignoreNotFound(c.do(ctx, http.MethodDelete, "/volumes/"+strconv.FormatInt(id, 10), nil, nil))
}

// PrimaryIP is a public address; a server's own is assigned to it and
// deleted with it.
type PrimaryIP struct {
	ID         int64             `json:"id"`
	IP         string            `json:"ip"`
	AssigneeID *int64            `json:"assignee_id"` // nil when unassigned
	Labels     map[string]string `json:"labels"`
	Created    time.Time         `json:"created"`
}

func (c *Client) PrimaryIP(ctx context.Context, id int64) (PrimaryIP, error) {
	var out struct {
		PrimaryIP PrimaryIP `json:"primary_ip"`
	}
	err := c.do(ctx, http.MethodGet, "/primary_ips/"+strconv.FormatInt(id, 10), nil, &out)
	return out.PrimaryIP, err
}

// PrimaryIPs lists the project's primary IPs matching labelSelector ("" =
// all).
func (c *Client) PrimaryIPs(ctx context.Context, labelSelector string) ([]PrimaryIP, error) {
	var all []PrimaryIP
	err := c.list(ctx, "/primary_ips", labelSelector, func(raw []byte) error {
		var page struct {
			PrimaryIPs []PrimaryIP `json:"primary_ips"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		all = append(all, page.PrimaryIPs...)
		return nil
	})
	return all, err
}

// DeletePrimaryIP is idempotent: an address already gone is success.
func (c *Client) DeletePrimaryIP(ctx context.Context, id int64) error {
	return ignoreNotFound(c.do(ctx, http.MethodDelete, "/primary_ips/"+strconv.FormatInt(id, 10), nil, nil))
}

type Price struct {
	Location     string `json:"location"`
	PriceHourly  Amount `json:"price_hourly"`
	PriceMonthly Amount `json:"price_monthly"`
}

// Amount holds Hetzner's decimal strings.
type Amount struct {
	Net   string `json:"net"`
	Gross string `json:"gross"`
}

type Pricing struct {
	Currency    string `json:"currency"`
	ServerTypes []struct {
		Name   string  `json:"name"`
		Prices []Price `json:"prices"`
	} `json:"server_types"`
	PrimaryIPs []struct {
		Type   string  `json:"type"` // ipv4 | ipv6
		Prices []Price `json:"prices"`
	} `json:"primary_ips"`
	Volume struct {
		PricePerGBMonth Amount `json:"price_per_gb_month"`
	} `json:"volume"`
}

func (c *Client) Pricing(ctx context.Context) (Pricing, error) {
	var out struct {
		Pricing Pricing `json:"pricing"`
	}
	err := c.do(ctx, http.MethodGet, "/pricing", nil, &out)
	return out.Pricing, err
}

// ServerHourlyNet is the net hourly price of serverType in location.
func (p Pricing) ServerHourlyNet(serverType, location string) (float64, error) {
	return p.server(serverType, location, hourly)
}

// ServerMonthlyNet is the net monthly price (the hourly billing's cap) of
// serverType in location.
func (p Pricing) ServerMonthlyNet(serverType, location string) (float64, error) {
	return p.server(serverType, location, monthly)
}

// PrimaryIPHourlyNet is the net hourly price of a primary IP of ipType
// (ipv4) in location.
func (p Pricing) PrimaryIPHourlyNet(ipType, location string) (float64, error) {
	return p.primaryIP(ipType, location, hourly)
}

// PrimaryIPMonthlyNet is the net monthly price of a primary IP of ipType.
func (p Pricing) PrimaryIPMonthlyNet(ipType, location string) (float64, error) {
	return p.primaryIP(ipType, location, monthly)
}

// VolumeGBMonthNet is the net price of one GB of volume for a month.
func (p Pricing) VolumeGBMonthNet() (float64, error) {
	if p.Volume.PricePerGBMonth.Net == "" {
		return 0, fmt.Errorf("hcloud pricing: no volume price")
	}
	return strconv.ParseFloat(p.Volume.PricePerGBMonth.Net, 64)
}

func hourly(pr Price) string  { return pr.PriceHourly.Net }
func monthly(pr Price) string { return pr.PriceMonthly.Net }

func (p Pricing) server(serverType, location string, field func(Price) string) (float64, error) {
	for _, st := range p.ServerTypes {
		if st.Name == serverType {
			return priceIn(st.Prices, location, "server type "+serverType, field)
		}
	}
	return 0, fmt.Errorf("hcloud pricing: no server type %s", serverType)
}

func (p Pricing) primaryIP(ipType, location string, field func(Price) string) (float64, error) {
	for _, ip := range p.PrimaryIPs {
		if ip.Type == ipType {
			return priceIn(ip.Prices, location, "primary ip "+ipType, field)
		}
	}
	return 0, fmt.Errorf("hcloud pricing: no primary ip type %s", ipType)
}

func priceIn(prices []Price, location, what string, field func(Price) string) (float64, error) {
	for _, pr := range prices {
		if pr.Location == location {
			return strconv.ParseFloat(field(pr), 64)
		}
	}
	return 0, fmt.Errorf("hcloud pricing: %s has no price in %s", what, location)
}
