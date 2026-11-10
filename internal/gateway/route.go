package gateway

import (
	"context"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The slow-alias rule: an alias whose rate over the last latencyWindow is
// above the policy's bound serves from its twin. The rate is the window's
// sum of call durations over its sum of output tokens, per 1000 tokens, of
// the calls its own deployments (LiteLLM's /model/info) answered, whichever
// alias name the call carried. Only calls of minLatencyTokens output tokens
// or more count. The rate that switches needs minLatencyCalls of them; the
// rate that returns needs minReturnCalls, and a switched alias with fewer
// sends its next call to the primary, at most once a routeTTL. A switched
// alias also sends every primaryEvery-th call to its primary. The window is
// the audit table, so a restart or a second replica reads the same one; the
// call counter is per replica.
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
// its twin and which, the calls since, and when the window was last read.
type aliasRoute struct {
	checked  time.Time
	reading  bool
	switched bool
	twin     string
	calls    int
	quiet    bool // the primary served too few counted calls to return
}

// routed is the alias that serves a call, a note for the audit row when
// that is its twin, and undo, which gives back what the call used of the
// probe schedule when the call is refused before it runs.
type routed struct {
	alias, note string
	undo        func()
}

// route answers the alias that serves a call for alias.
func (s *Server) route(ctx context.Context, pv *policy.Version, alias string) routed {
	routing := pv.Document.Routing
	twin := routing.Twin(alias)
	r, ok := s.routeOf(ctx, alias)
	if !ok {
		return routed{alias: alias}
	}
	s.routeMu.Lock()
	stale := r.switched && r.twin != twin
	reread := twin != "" && !r.reading && s.now().Sub(r.checked) >= routeTTL
	if stale || reread {
		r.reading, r.checked = true, s.now()
	}
	was := *r
	s.routeMu.Unlock()

	var closed bool
	var verdict routeVerdict
	switch {
	case stale:
		closed = s.closeSwitch(ctx, alias, was.twin)
	case reread:
		verdict = s.judge(ctx, alias, twin, was.switched, routing.Bound())
	}

	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if stale || reread {
		r.reading = false
	}
	if closed {
		r.switched, r.calls, r.quiet = false, 0, false
	}
	if verdict.read {
		r.quiet = r.switched && verdict.counted < minReturnCalls
		if verdict.move != nil {
			r.switched, r.twin, r.calls = verdict.move.Kind == policy.SwitchOn, twin, 0
			r.quiet = r.switched && verdict.counted < minReturnCalls
		}
	}
	if !r.switched || twin == "" || r.twin != twin {
		return routed{alias: alias}
	}
	if r.quiet {
		r.quiet = false
		return routed{alias: alias, undo: func() { s.routeMu.Lock(); r.quiet = true; s.routeMu.Unlock() }}
	}
	undo := func() { s.routeMu.Lock(); r.calls--; s.routeMu.Unlock() }
	if r.calls++; r.calls%primaryEvery == 0 {
		return routed{alias: alias, undo: undo}
	}
	return routed{alias: twin, note: fmt.Sprintf("%s is slow: served on its twin %s", alias, twin), undo: undo}
}

// routeOf is alias's state, the open switches read once on first use.
func (s *Server) routeOf(ctx context.Context, alias string) (*aliasRoute, bool) {
	s.routeMu.Lock()
	loaded := s.routes != nil
	s.routeMu.Unlock()
	var open []policy.AliasSwitch
	if !loaded {
		ctx, cancel := context.WithTimeout(ctx, routeReadTimeout)
		defer cancel()
		var err error
		if open, err = s.opts.Store.OpenAliasSwitches(ctx); err != nil {
			log.Printf("read the open alias switches: %v", err)
			return nil, false
		}
	}
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if s.routes == nil {
		s.routes = map[string]*aliasRoute{}
		for _, sw := range open {
			s.routes[sw.Alias] = &aliasRoute{switched: true, twin: sw.Twin}
		}
	}
	r := s.routes[alias]
	if r == nil {
		r = &aliasRoute{}
		s.routes[alias] = r
	}
	return r, true
}

// closeSwitch stores the return of an alias whose policy names another twin
// now, or none.
func (s *Server) closeSwitch(ctx context.Context, alias, twin string) bool {
	ctx, cancel := context.WithTimeout(ctx, routeReadTimeout)
	defer cancel()
	sw := policy.AliasSwitch{Alias: alias, Twin: twin, Kind: policy.SwitchReturn}
	if err := s.opts.Store.RecordAliasSwitch(ctx, sw); err != nil {
		log.Printf("close the switch of %s to %s: %v", alias, twin, err)
		return false
	}
	log.Printf("alias %s: the policy no longer names %s as its twin; back on %s", alias, twin, alias)
	return true
}

// routeVerdict is what one read of the window found: whether it was read,
// how many calls counted, and the move it called for, already stored.
type routeVerdict struct {
	read    bool
	counted int
	move    *policy.AliasSwitch
}

// judge reads the alias's window and stores a move to its twin or back to
// its primary when the rate says so. A twin LiteLLM cannot price is no move.
func (s *Server) judge(ctx context.Context, alias, twin string, switched bool, bound float64) routeVerdict {
	ctx, cancel := context.WithTimeout(ctx, routeReadTimeout)
	defer cancel()
	deployments, err := s.opts.Prices.Deployments(ctx, alias)
	if err != nil {
		log.Printf("read the deployments of %s: %v", alias, err)
		return routeVerdict{}
	}
	samples, err := s.opts.Store.LatencySamples(ctx, []string{alias, twin}, s.now().Add(-latencyWindow))
	if err != nil {
		log.Printf("read the latency of %s: %v", alias, err)
		return routeVerdict{}
	}
	l := latencyOf(deployments, samples)
	v := routeVerdict{read: true, counted: l.counted}
	kind := ""
	switch {
	case !switched && l.counted >= minLatencyCalls && l.rate > bound:
		kind = policy.SwitchOn
		if _, err := s.opts.Prices.Price(ctx, twin); err != nil {
			log.Printf("alias %s is slow (%.1f s per 1000 output tokens, bound %g) but stays: its twin %s cannot be priced: %v", alias, l.rate, bound, twin, err)
			return v
		}
	case switched && l.counted >= minReturnCalls && l.rate <= bound:
		kind = policy.SwitchReturn
	default:
		return v
	}
	sw := policy.AliasSwitch{Alias: alias, Twin: twin, Kind: kind, Rate: l.rate, Deployment: l.deployment}
	if err := s.opts.Store.RecordAliasSwitch(ctx, sw); err != nil {
		log.Printf("record the %s of %s: %v", kind, alias, err)
		return v
	}
	log.Printf("alias %s: %s at %.1f s per 1000 output tokens (bound %g)", alias, kind, l.rate, bound)
	v.move = &sw
	return v
}

// latency is the window of an alias's own deployments: how many calls they
// answered, how many of minLatencyTokens or more, those calls' seconds per
// 1000 output tokens, and the deployment that answered most.
type latency struct {
	counted    int
	rate       float64
	deployment string
}

// latencyOf reads the window's samples of the calls that one of deployments
// answered: LiteLLM's names are provider/model and the header may carry the
// model alone, so a deployment is compared by its model.
func latencyOf(deployments []string, samples []policy.LatencySample) latency {
	own := map[string]bool{}
	for _, d := range deployments {
		own[modelOf(d)] = true
	}
	var l latency
	var ms, tokens int
	counts := map[string]int{}
	for _, s := range samples {
		if own[modelOf(s.Deployment)] && s.OutputTokens >= minLatencyTokens {
			l.counted++
			ms, tokens = ms+s.DurationMS, tokens+s.OutputTokens
			counts[s.Deployment]++
		}
	}
	if l.counted == 0 {
		return l
	}
	l.rate = float64(ms) / float64(tokens)
	for _, d := range slices.Sorted(maps.Keys(counts)) {
		if counts[d] > counts[l.deployment] {
			l.deployment = d
		}
	}
	return l
}

// modelOf is the model of a deployment: what follows its last slash.
func modelOf(deployment string) string {
	return deployment[strings.LastIndex(deployment, "/")+1:]
}
