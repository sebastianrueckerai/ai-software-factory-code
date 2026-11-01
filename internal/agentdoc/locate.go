// Package agentdoc renders the factory's agent context map: it captures each
// agent's first model request through the real code, labels every part of it
// by the builder that made it, and emits one page per agent. It renders no
// verdict — sizes, provenance, duplication and what stayed unlabelled are all
// it says.
package agentdoc

import (
	"fmt"
	"sort"
	"strings"
)

// Kind is a part's origin: a Go builder, the opencode harness, a per-repo
// file, a gateway insertion, the policy, or fixture data standing in for a
// live card.
const (
	KindBuilder = "builder"
	KindHarness = "harness"
	KindFile    = "file"
	KindGateway = "gateway"
	KindPolicy  = "policy"
	KindFixture = "fixture"
)

// Place is where a span is located: the system message's decoded content,
// the user message's (a structured part list's texts joined), or the raw
// body bytes (a tool schema json.Marshal wrote).
const (
	PlaceSystem = "system"
	PlaceUser   = "user"
	PlaceTools  = "tools"
)

// Span is one named part of a captured request: the output a registered
// builder produced on the capture's fixture. Locating spans inside the
// captured request is the map's provenance; a span that no longer appears,
// or appears twice, is the map rotting and fails the capture.
type Span struct {
	Name   string
	Symbol string // the Go symbol, for the permalink
	Kind   string
	Place  string
	Text   string
}

// Claim is a span located inside a captured request, with its byte range:
// Start and End are offsets in the string (or raw body) it was located in.
// Within holds spans nested inside this one's range. A claim with no Span
// names bytes no registered builder claims.
type Claim struct {
	Span
	Start, End int
	Unlabelled bool
	Within     []Claim
}

// Locate partitions s into claims, longest span first: every span's text must
// appear in s exactly once; bytes no top-level span covers come back as an
// unlabelled claim. Spans nested inside another's range nest as Within.
func Locate(s string, spans []Span) ([]Claim, error) {
	return locate([]byte(s), spans)
}

// LocateRaw is Locate over the raw request body, where structured parts (a
// tool schema json.Marshal wrote inside the body) match byte for byte.
func LocateRaw(raw []byte, spans []Span) ([]Claim, error) {
	return locate(raw, spans)
}

func locate(b []byte, spans []Span) ([]Claim, error) {
	type hit struct {
		span  Span
		start int
	}
	var hits []hit
	byLen := make([]Span, 0, len(spans))
	for _, sp := range spans {
		if sp.Text != "" {
			byLen = append(byLen, sp)
		}
	}
	sort.Slice(byLen, func(i, j int) bool { return len(byLen[i].Text) > len(byLen[j].Text) })
	for _, sp := range byLen {
		first := strings.Index(string(b), sp.Text)
		if first < 0 {
			return nil, fmt.Errorf("span %s (%s) not found in the captured request", sp.Name, sp.Symbol)
		}
		if strings.LastIndex(string(b), sp.Text) != first {
			return nil, fmt.Errorf("span %s (%s) matches twice in the captured request", sp.Name, sp.Symbol)
		}
		hits = append(hits, hit{span: sp, start: first})
	}
	// a hit inside another hit's range nests under it; the rest are top level
	nested := map[int]bool{}
	for i, h := range hits {
		for j, o := range hits {
			if i != j && h.start >= o.start && h.start+len(h.span.Text) <= o.start+len(o.span.Text) {
				nested[i] = true
				break
			}
		}
	}
	var top []Claim
	for i, h := range hits {
		if nested[i] {
			continue
		}
		top = append(top, Claim{Span: h.span, Start: h.start, End: h.start + len(h.span.Text)})
	}
	for i, h := range hits {
		if !nested[i] {
			continue
		}
		for j, c := range top {
			if h.start >= c.Start && h.start+len(h.span.Text) <= c.End {
				top[j].Within = append(top[j].Within, Claim{Span: h.span, Start: h.start, End: h.start + len(h.span.Text)})
			}
		}
	}
	sort.SliceStable(top, func(i, j int) bool { return top[i].Start < top[j].Start })
	// bytes between top claims are unlabelled
	var out []Claim
	at := 0
	for _, c := range top {
		if c.Start > at {
			out = append(out, Claim{Start: at, End: c.Start, Unlabelled: true})
		}
		out = append(out, c)
		at = c.End
	}
	if at < len(b) {
		out = append(out, Claim{Start: at, End: len(b), Unlabelled: true})
	}
	for i := range out {
		if out[i].Unlabelled {
			out[i].Text = string(b[out[i].Start:out[i].End])
		}
	}
	return out, nil
}
