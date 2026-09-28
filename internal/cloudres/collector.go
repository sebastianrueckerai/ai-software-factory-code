package cloudres

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/hcloud"
)

// Provider is the slice of the Hetzner API the collector needs; the token
// behind it decides the project.
type Provider interface {
	Servers(ctx context.Context, labelSelector string) ([]hcloud.Server, error)
	Server(ctx context.Context, id int64) (hcloud.Server, error)
	DeleteServer(ctx context.Context, id int64) error
	Firewalls(ctx context.Context, labelSelector string) ([]hcloud.Firewall, error)
	Firewall(ctx context.Context, id int64) (hcloud.Firewall, error)
	DeleteFirewall(ctx context.Context, id int64) error
	Volumes(ctx context.Context, labelSelector string) ([]hcloud.Volume, error)
	Volume(ctx context.Context, id int64) (hcloud.Volume, error)
	DetachVolume(ctx context.Context, id int64) error
	DeleteVolume(ctx context.Context, id int64) error
	PrimaryIPs(ctx context.Context, labelSelector string) ([]hcloud.PrimaryIP, error)
	DeletePrimaryIP(ctx context.Context, id int64) error
}

// Collector deletes the project's resources whose lease expired or whose
// owner finished (promoted ones excepted), deletes every server, firewall
// and volume in the provider project the registry does not know as open
// and every primary IP no server holds, and closes rows whose resource is
// gone. It owns the whole provider project: nothing else may live there.
type Collector struct {
	Project  string // ci | agent
	Registry Registry
	Provider Provider
	// NodeIPs are the cluster nodes' public IPv4s (addresses or CIDRs,
	// cluster-facts): a listed server on one of them means the token opens
	// the cluster's own project, and the pass refuses (ruling 123).
	NodeIPs []string
	// OwnerDone reports whether a resource's owner (ci: the Forgejo job) is
	// finished; nil means owners never finish early and only leases end.
	OwnerDone func(ctx context.Context, ownerRef string) (bool, error)
	// RequestedGrace is how long a requested row may lack its provider
	// resource before it counts as never created. 0 = 10 minutes.
	RequestedGrace time.Duration
	Interval       time.Duration // 0 = 1 minute
	// RetryWait spaces Release's retries of a firewall or volume its
	// deleting server still holds. 0 = 3 seconds.
	RetryWait time.Duration
	// ReleaseAttempts bounds Release's tries per resource (0 = 10). An
	// owner releasing from a loop that must not stall sets 1: what is still
	// held stays deleting for the next pass.
	ReleaseAttempts int
	Now             func() time.Time
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Run collects every Interval until ctx ends.
func (c *Collector) Run(ctx context.Context) {
	interval := c.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := c.Collect(ctx); err != nil {
			log.Printf("cloud collector (%s): %v", c.Project, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

type providerResource struct {
	kind   string
	id     int64
	labels map[string]string
}

// Collect runs one pass. The provider is listed BEFORE the registry is
// read: a resource in the listing was created after its row was written,
// so the row read afterwards always knows it, and a create racing this
// pass is never mistaken for an unknown resource. Primary IPs have no
// rows: a server's own address goes with the server, so only an address
// no server holds is collected.
func (c *Collector) Collect(ctx context.Context) error {
	servers, err := c.Provider.Servers(ctx, "")
	if err != nil {
		return fmt.Errorf("list servers: %w", err)
	}
	if err := c.interlock(servers); err != nil {
		log.Printf("cloud collector (%s): refusing the pass: %v", c.Project, err)
		return fmt.Errorf("refusing the pass: %w", err)
	}
	firewalls, err := c.Provider.Firewalls(ctx, "")
	if err != nil {
		return fmt.Errorf("list firewalls: %w", err)
	}
	volumes, err := c.Provider.Volumes(ctx, "")
	if err != nil {
		return fmt.Errorf("list volumes: %w", err)
	}
	ips, err := c.Provider.PrimaryIPs(ctx, "")
	if err != nil {
		return fmt.Errorf("list primary ips: %w", err)
	}
	listed := make([]providerResource, 0, len(servers)+len(firewalls)+len(volumes))
	for _, s := range servers {
		listed = append(listed, providerResource{KindServer, s.ID, s.Labels})
	}
	for _, f := range firewalls {
		listed = append(listed, providerResource{KindFirewall, f.ID, f.Labels})
	}
	for _, v := range volumes {
		listed = append(listed, providerResource{KindVolume, v.ID, v.Labels})
	}
	rows, err := c.Registry.Open(ctx, c.Project)
	if err != nil {
		return fmt.Errorf("registry: %w", err)
	}

	var errs []error
	matched := map[providerKey]bool{}
	ownerDone := map[string]bool{}
	for _, kind := range Kinds {
		for _, row := range rows {
			if row.Kind != kind {
				continue
			}
			found, ok := match(row, listed)
			if ok {
				matched[key(found)] = true
			}
			if err := c.collectRow(ctx, row, found, ok, ownerDone); err != nil {
				errs = append(errs, fmt.Errorf("resource %d (%s %s): %w", row.ID, row.Kind, row.Name, err))
			}
		}
	}
	for _, kind := range Kinds {
		for _, p := range listed {
			if p.kind != kind || matched[key(p)] {
				continue
			}
			log.Printf("cloud collector (%s): deleting unknown %s %d (labels %v)", c.Project, p.kind, p.id, p.labels)
			if err := c.deleteProvider(ctx, p.kind, p.id); err != nil {
				errs = append(errs, fmt.Errorf("unknown %s %d: %w", p.kind, p.id, err))
			}
		}
	}
	for _, ip := range ips {
		if ip.AssigneeID != nil {
			continue
		}
		log.Printf("cloud collector (%s): deleting unassigned primary ip %d (%s)", c.Project, ip.ID, ip.IP)
		if err := c.Provider.DeletePrimaryIP(ctx, ip.ID); err != nil {
			errs = append(errs, fmt.Errorf("primary ip %d: %w", ip.ID, err))
		}
	}
	return errors.Join(errs...)
}

// interlock is ruling 123: the collector deletes whatever its project
// holds, so a token that opens another project (the cluster's, another
// factory project's) must stop it before anything is deleted.
func (c *Collector) interlock(servers []hcloud.Server) error {
	if len(c.NodeIPs) == 0 {
		return errors.New("no cluster node IPs to check the project against")
	}
	var nodes []*net.IPNet
	for _, n := range c.NodeIPs {
		n = strings.TrimSpace(n)
		if !strings.Contains(n, "/") {
			n += "/32"
		}
		_, cidr, err := net.ParseCIDR(n)
		if err != nil {
			return fmt.Errorf("node ip %q: %w", n, err)
		}
		nodes = append(nodes, cidr)
	}
	for _, s := range servers {
		if p, ok := s.Labels["factory"]; ok && p != c.Project {
			return fmt.Errorf("server %d carries factory=%s: the token opens another project", s.ID, p)
		}
		ip := net.ParseIP(s.IPv4())
		for _, n := range nodes {
			if ip != nil && n.Contains(ip) {
				return fmt.Errorf("server %d is on cluster node address %s: the token opens the cluster's project", s.ID, ip)
			}
		}
	}
	return nil
}

type providerKey struct {
	kind string
	id   int64
}

func key(p providerResource) providerKey { return providerKey{p.kind, p.id} }

// match finds a row's provider resource: by provider id once recorded,
// else by the resource label the row's id put on it.
func match(row Resource, listed []providerResource) (providerResource, bool) {
	for _, p := range listed {
		if p.kind != row.Kind {
			continue
		}
		if row.ProviderID != "" {
			if strconv.FormatInt(p.id, 10) == row.ProviderID {
				return p, true
			}
			continue
		}
		if p.labels["resource"] == strconv.FormatInt(row.ID, 10) && p.labels["factory"] == row.Project {
			return p, true
		}
	}
	return providerResource{}, false
}

func (c *Collector) collectRow(ctx context.Context, row Resource, found providerResource, ok bool, ownerDone map[string]bool) error {
	now := c.now()
	if !ok {
		return c.closeAbsent(ctx, row, now)
	}
	if row.State == StateRequested {
		if _, err := c.Registry.SetLive(ctx, row.ID, strconv.FormatInt(found.id, 10)); err != nil {
			return err
		}
		row.State = StateLive
	}
	reason := ""
	switch {
	case row.State == StateDeleting:
		reason = "deletion pending"
	case row.Promoted:
	case !now.Before(row.LeaseExpires):
		reason = "lease expired"
	default:
		done, err := c.ownerDone(ctx, row.OwnerRef, ownerDone)
		if err != nil {
			return fmt.Errorf("owner %s: %w", row.OwnerRef, err)
		}
		if done {
			reason = "owner " + row.OwnerRef + " finished"
		}
	}
	if reason == "" {
		return nil
	}
	log.Printf("cloud collector (%s): deleting %s %d (resource %d): %s", c.Project, row.Kind, found.id, row.ID, reason)
	return c.Delete(ctx, row, found.id)
}

func (c *Collector) ownerDone(ctx context.Context, owner string, cache map[string]bool) (bool, error) {
	if c.OwnerDone == nil {
		return false, nil
	}
	if done, ok := cache[owner]; ok {
		return done, nil
	}
	done, err := c.OwnerDone(ctx, owner)
	if err != nil {
		return false, err
	}
	cache[owner] = done
	return done, nil
}

// closeAbsent handles a row whose resource is not in the listing. A listing
// can lag, so a row that had a provider id is only closed once a direct
// read answers 404.
func (c *Collector) closeAbsent(ctx context.Context, row Resource, now time.Time) error {
	if row.ProviderID == "" {
		grace := c.RequestedGrace
		if grace <= 0 {
			grace = 10 * time.Minute
		}
		if now.Sub(row.CreatedAt) < grace {
			return nil // a create may still be in flight
		}
		_, err := c.Registry.Transition(ctx, row.ID, StateVanished)
		return err
	}
	id, err := strconv.ParseInt(row.ProviderID, 10, 64)
	if err != nil {
		return fmt.Errorf("provider id %q: %w", row.ProviderID, err)
	}
	gone, err := c.gone(ctx, row.Kind, id)
	if err != nil || !gone {
		return err
	}
	to := StateVanished
	if row.State == StateDeleting {
		to = StateDeleted
	}
	_, err = c.Registry.Transition(ctx, row.ID, to)
	return err
}

func (c *Collector) gone(ctx context.Context, kind string, id int64) (bool, error) {
	var err error
	switch kind {
	case KindServer:
		_, err = c.Provider.Server(ctx, id)
	case KindFirewall:
		_, err = c.Provider.Firewall(ctx, id)
	case KindVolume:
		_, err = c.Provider.Volume(ctx, id)
	default:
		return false, fmt.Errorf("unsupported kind %s", kind)
	}
	if errors.Is(err, hcloud.ErrNotFound) {
		return true, nil
	}
	return false, err
}

// Delete deletes row's provider resource and closes the row: deleting is
// recorded before the call, deleted after it succeeds (a 404 counts). A
// failed call leaves the row deleting for the next pass. Shared with the
// owners' own teardown.
func (c *Collector) Delete(ctx context.Context, row Resource, providerID int64) error {
	if _, err := c.Registry.Transition(ctx, row.ID, StateDeleting); err != nil {
		return err
	}
	if err := c.deleteProvider(ctx, row.Kind, providerID); err != nil {
		return err
	}
	_, err := c.Registry.Transition(ctx, row.ID, StateDeleted)
	return err
}

// ErrDetaching is a volume delete waiting for its detach.
var ErrDetaching = errors.New("volume still attached: detach started, delete on the next attempt")

func (c *Collector) deleteProvider(ctx context.Context, kind string, id int64) error {
	switch kind {
	case KindServer:
		return c.Provider.DeleteServer(ctx, id)
	case KindFirewall:
		return c.Provider.DeleteFirewall(ctx, id)
	case KindVolume:
		v, err := c.Provider.Volume(ctx, id)
		if errors.Is(err, hcloud.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if v.Server != nil {
			if err := c.Provider.DetachVolume(ctx, id); err != nil {
				return err
			}
			return ErrDetaching
		}
		return c.Provider.DeleteVolume(ctx, id)
	}
	return fmt.Errorf("unsupported kind %s", kind)
}

// Release deletes the rows' resources now, servers first, and closes the
// rows: the owner's own teardown. A firewall or volume its deleting server
// still holds is retried a few times; past that its row stays deleting for
// the collector.
func (c *Collector) Release(ctx context.Context, rows []Resource) error {
	var errs []error
	for _, kind := range Kinds {
		for _, row := range rows {
			if row.Kind != kind {
				continue
			}
			if err := c.release(ctx, row); err != nil {
				errs = append(errs, fmt.Errorf("%s %s: %w", row.Kind, row.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (c *Collector) release(ctx context.Context, row Resource) error {
	id, found, err := c.providerID(ctx, row)
	if err != nil {
		return err
	}
	if !found {
		// Never created: a create that is still in flight or lands later
		// finds its row closed, and the collector deletes it as unknown.
		_, err := c.Registry.Transition(ctx, row.ID, StateDeleted)
		return err
	}
	wait := c.RetryWait
	if wait <= 0 {
		wait = 3 * time.Second
	}
	attempts := c.ReleaseAttempts
	if attempts <= 0 {
		attempts = 10
	}
	for attempt := 1; ; attempt++ {
		err := c.Delete(ctx, row, id)
		if err == nil || !(hcloud.IsCode(err, "resource_in_use") || errors.Is(err, ErrDetaching)) || attempt >= attempts {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// providerID is the row's recorded provider id, or the id of the resource
// carrying its label when the create answered but the id was not recorded.
func (c *Collector) providerID(ctx context.Context, row Resource) (int64, bool, error) {
	if row.ProviderID != "" {
		id, err := strconv.ParseInt(row.ProviderID, 10, 64)
		return id, err == nil, err
	}
	selector := "resource=" + strconv.FormatInt(row.ID, 10)
	var ids []int64
	switch row.Kind {
	case KindServer:
		found, err := c.Provider.Servers(ctx, selector)
		if err != nil {
			return 0, false, err
		}
		for _, s := range found {
			ids = append(ids, s.ID)
		}
	case KindFirewall:
		found, err := c.Provider.Firewalls(ctx, selector)
		if err != nil {
			return 0, false, err
		}
		for _, f := range found {
			ids = append(ids, f.ID)
		}
	case KindVolume:
		found, err := c.Provider.Volumes(ctx, selector)
		if err != nil {
			return 0, false, err
		}
		for _, v := range found {
			ids = append(ids, v.ID)
		}
	default:
		return 0, false, fmt.Errorf("unsupported kind %s", row.Kind)
	}
	if len(ids) == 0 {
		return 0, false, nil
	}
	return ids[0], true, nil
}
