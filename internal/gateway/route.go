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
// median that switches needs minLatencyCalls of them; the median that
// returns needs minReturnCalls, and a switched alias whose primary served no
// call in the window sends its next call there. A switched alias still sends every
// primaryEvery-th call to its primary, so the window keeps measuring it. The
// window is the audit table, so a restart or a second replica reads the
// same one; the call counter is per replica.
const (
	latencyWindow    = time.Hour
	minLatencyCalls  = 20
	minReturnCalls   = 3
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
	quiet    bool // the primary served no call in the window
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
	if r.quiet {
		r.quiet = false
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
	l := latencyOf(alias, twin, samples)
	r.quiet = r.switched && l.primary == 0
	kind := ""
	switch {
	case !r.switched && l.counted >= minLatencyCalls && l.median > bound:
		kind = policy.SwitchOn
	case r.switched && l.counted >= minReturnCalls && l.median <= bound:
		kind = policy.SwitchReturn
	default:
		return
	}
	median, deployment := l.median, l.deployment
	sw := policy.AliasSwitch{Alias: alias, Twin: twin, Kind: kind, Median: median, Deployment: deployment}
	if err := s.opts.Store.RecordAliasSwitch(ctx, sw); err != nil {
		log.Printf("record the %s of %s: %v", kind, alias, err)
		return
	}
	r.switched, r.calls = kind == policy.SwitchOn, 0
	log.Printf("alias %s: %s at a median of %.1f s per 1000 output tokens (bound %g)", alias, kind, median, bound)
}

// latency is the window of an alias's primary deployment: how many calls it
// answered, how many of minLatencyTokens or more, their median seconds per
// 1000 output tokens, and the deployment that answered most.
type latency struct {
	primary, counted int
	median           float64
	deployment       string
}

// latencyOf reads the window's samples. A deployment that answered a call of
// the twin is no primary: LiteLLM's fallback served that call of alias.
func latencyOf(alias, twin string, samples []policy.LatencySample) latency {
	twins := map[string]bool{}
	for _, s := range samples {
		if s.Alias == twin {
			twins[s.Deployment] = true
		}
	}
	var l latency
	var rates []float64
	deployments := map[string]int{}
	for _, s := range samples {
		if s.Alias != alias || twins[s.Deployment] {
			continue
		}
		l.primary++
		if s.OutputTokens >= minLatencyTokens {
			rates = append(rates, float64(s.DurationMS)/float64(s.OutputTokens))
			deployments[s.Deployment]++
		}
	}
	if l.counted = len(rates); l.counted == 0 {
		return l
	}
	slices.Sort(rates)
	l.median = rates[len(rates)/2]
	if len(rates)%2 == 0 {
		l.median = (rates[len(rates)/2-1] + l.median) / 2
	}
	for d, n := range deployments {
		if n > deployments[l.deployment] || n == deployments[l.deployment] && d < l.deployment {
			l.deployment = d
		}
	}
	return l
}
