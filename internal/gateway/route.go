package gateway

import (
	"context"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The slow-alias rule: an alias whose median over the last latencyWindow of
// the primary deployment's calls is above the policy's bound serves from its
// twin. Only calls of minLatencyTokens output tokens or more count, and the
// median needs minLatencyCalls of them. A switched alias still sends every
// primaryEvery-th call to its primary, so the window keeps measuring it. The
// window is the audit table, so a restart or a second replica reads the
// same one; the call counter is per replica.
const (
	latencyWindow    = time.Hour
	minLatencyCalls  = 20
	minLatencyTokens = 100
	primaryEvery     = 10
	routeTTL         = time.Minute
	routeReadTimeout = 10 * time.Second
)

// aliasRoute is what the gateway holds of one alias: whether it serves from
// its twin, the calls since, and when the window was last read.
type aliasRoute struct {
	checked  time.Time
	switched bool
	calls    int
}

// route answers the alias that serves a call for alias, and a note for the
// audit row when that is its twin.
func (s *Server) route(ctx context.Context, pv *policy.Version, alias string) (string, string) {
	routing := pv.Document.Routing
	twin := routing.Twin(alias)
	if twin == "" {
		return alias, ""
	}
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if s.routes == nil {
		open, err := s.opts.Store.OpenAliasSwitches(ctx)
		if err != nil {
			log.Printf("read the open alias switches: %v", err)
			return alias, ""
		}
		s.routes = map[string]*aliasRoute{}
		for _, sw := range open {
			s.routes[sw.Alias] = &aliasRoute{switched: true}
		}
	}
	r := s.routes[alias]
	if r == nil {
		r = &aliasRoute{}
		s.routes[alias] = r
	}
	if s.now().Sub(r.checked) >= routeTTL {
		s.reread(ctx, alias, twin, routing.Bound(), r)
	}
	if !r.switched {
		return alias, ""
	}
	if r.calls++; r.calls%primaryEvery == 0 {
		return alias, ""
	}
	return twin, fmt.Sprintf("%s is slow: served on its twin %s", alias, twin)
}

// reread reads the alias's window and moves it to its twin or back to its
// primary when the median says so, recording the move.
func (s *Server) reread(ctx context.Context, alias, twin string, bound float64, r *aliasRoute) {
	r.checked = s.now()
	ctx, cancel := context.WithTimeout(ctx, routeReadTimeout)
	defer cancel()
	samples, err := s.opts.Store.LatencySamples(ctx, []string{alias, twin}, s.now().Add(-latencyWindow))
	if err != nil {
		log.Printf("read the latency of %s: %v", alias, err)
		return
	}
	median, deployment, ok := latencyMedian(alias, twin, samples)
	if !ok {
		return
	}
	kind := ""
	switch {
	case !r.switched && median > bound:
		kind = policy.SwitchOn
	case r.switched && median <= bound:
		kind = policy.SwitchReturn
	default:
		return
	}
	sw := policy.AliasSwitch{Alias: alias, Twin: twin, Kind: kind, Median: median, Deployment: deployment}
	if err := s.opts.Store.RecordAliasSwitch(ctx, sw); err != nil {
		log.Printf("record the %s of %s: %v", kind, alias, err)
		return
	}
	r.switched, r.calls = kind == policy.SwitchOn, 0
	log.Printf("alias %s: %s at a median of %.1f s per 1000 output tokens (bound %g)", alias, kind, median, bound)
}

// latencyMedian is the median seconds per 1000 output tokens of alias's
// calls that its primary deployment answered, and that deployment (the most
// frequent), when at least minLatencyCalls calls of minLatencyTokens count.
// A deployment that answered a call of the twin is no primary: LiteLLM's
// fallback served that call of alias.
func latencyMedian(alias, twin string, samples []policy.LatencySample) (float64, string, bool) {
	twins := map[string]bool{}
	for _, s := range samples {
		if s.Alias == twin {
			twins[s.Deployment] = true
		}
	}
	var rates []float64
	deployments := map[string]int{}
	for _, s := range samples {
		if s.Alias == alias && !twins[s.Deployment] && s.OutputTokens >= minLatencyTokens {
			rates = append(rates, float64(s.DurationMS)/float64(s.OutputTokens))
			deployments[s.Deployment]++
		}
	}
	if len(rates) < minLatencyCalls {
		return 0, "", false
	}
	slices.Sort(rates)
	median := rates[len(rates)/2]
	if len(rates)%2 == 0 {
		median = (rates[len(rates)/2-1] + median) / 2
	}
	top := ""
	for d, n := range deployments {
		if n > deployments[top] || n == deployments[top] && d < top {
			top = d
		}
	}
	return median, top, true
}
