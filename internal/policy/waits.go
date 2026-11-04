package policy

import (
	"cmp"
	"encoding/json"
	"fmt"
	"time"
)

// Waits are the bounds of the waits the AI owns: a wait past its bound
// opens a case. A wait at the human has no bound here.
type Waits struct {
	// Queued is how long a card may stay queued with its admission
	// reason. Default 2h.
	Queued time.Duration `yaml:"queued,omitempty" json:"queued,omitempty"`
	// Dependency is how long a card may wait on the cards it depends on.
	// Default 48h.
	Dependency time.Duration `yaml:"dependency,omitempty" json:"dependency,omitempty"`
	// Gate is how long a card may wait in review on a gate other than the
	// approval. Default 6h.
	Gate time.Duration `yaml:"gate,omitempty" json:"gate,omitempty"`
	// MergeRetry is how long a failed merge may wait for its retry.
	// Default 1h.
	MergeRetry time.Duration `yaml:"merge_retry,omitempty" json:"merge_retry,omitempty"`
	// ProviderBackoff is how long a card may wait after a provider refused
	// its session. Default 30m.
	ProviderBackoff time.Duration `yaml:"provider_backoff,omitempty" json:"provider_backoff,omitempty"`
	// RunQuiet is how long a running card may go without a step.
	// Default 30m.
	RunQuiet time.Duration `yaml:"run_quiet,omitempty" json:"run_quiet,omitempty"`
	// RungAnswer is how long a rung may take to answer a case step.
	// Default 35m.
	RungAnswer time.Duration `yaml:"rung_answer,omitempty" json:"rung_answer,omitempty"`
	// NoPushSessions is how many sessions in a row may end without a push.
	// At least 1. Default 3.
	NoPushSessions int `yaml:"no_push_sessions,omitempty" json:"no_push_sessions,omitempty"`
}

// DefaultWaits fills what the files leave out, bound by bound.
var DefaultWaits = Waits{Queued: 2 * time.Hour, Dependency: 48 * time.Hour, Gate: 6 * time.Hour, MergeRetry: time.Hour,
	ProviderBackoff: 30 * time.Minute, RunQuiet: 30 * time.Minute, RungAnswer: 35 * time.Minute, NoPushSessions: 3}

// maxWait is the longest bound a file may give a wait.
const maxWait = 7 * 24 * time.Hour

// waitBound is one timed bound of a Waits by its key.
type waitBound struct {
	key string
	d   *time.Duration
}

// bounds are w's timed bounds by their keys.
func (w *Waits) bounds() [7]waitBound {
	return [7]waitBound{{"queued", &w.Queued}, {"dependency", &w.Dependency}, {"gate", &w.Gate}, {"merge_retry", &w.MergeRetry},
		{"provider_backoff", &w.ProviderBackoff}, {"run_quiet", &w.RunQuiet}, {"rung_answer", &w.RungAnswer}}
}

// Bound is the bound of one wait kind by its key, such as "queued" or
// "rung_answer". 0 for an unknown kind.
func (w Waits) Bound(kind string) time.Duration {
	for _, b := range w.bounds() {
		if b.key == kind {
			return *b.d
		}
	}
	return 0
}

// WaitsOf is repository's wait bounds: DefaultWaits, policy.yaml's over
// them, then each of the project file's that is shorter.
func (t Tree) WaitsOf(repository string) Waits {
	_, p, _ := t.ProjectOf(repository)
	return t.Document.waitsOf(p)
}

func (d Document) waitsOf(p ProjectConfig) Waits {
	w, proj := DefaultWaits, cmp.Or(p.Waits, &Waits{})
	doc, pb := d.Waits.bounds(), proj.bounds()
	for i, b := range w.bounds() {
		*b.d = stricter(*b.d, *doc[i].d, *pb[i].d)
	}
	w.NoPushSessions = stricter(w.NoPushSessions, d.Waits.NoPushSessions, proj.NoPushSessions)
	return w
}

// stricter is a bound from its default, policy.yaml's value over it, and
// the project's when lower. A 0 is absent.
func stricter[T int | time.Duration](def, doc, proj T) T {
	v := cmp.Or(doc, def)
	return min(cmp.Or(proj, v), v)
}

// validate is the block's own judgement, as a file sets it: each bound 0
// or between 1m and 7 days, no_push_sessions not negative.
func (w Waits) validate() error {
	for _, b := range w.bounds() {
		if v := *b.d; v != 0 && (v < time.Minute || v > maxWait) {
			return fmt.Errorf("waits.%s must be 0 for the default or lie between 1m and 7 days, is %s", b.key, v)
		}
	}
	if w.NoPushSessions < 0 {
		return fmt.Errorf("waits.no_push_sessions must not be negative, is %d", w.NoPushSessions)
	}
	return nil
}

// waitsJSON is Waits with its bounds as strings, as in the YAML.
type waitsJSON struct {
	Queued          string `json:"queued,omitempty"`
	Dependency      string `json:"dependency,omitempty"`
	Gate            string `json:"gate,omitempty"`
	MergeRetry      string `json:"merge_retry,omitempty"`
	ProviderBackoff string `json:"provider_backoff,omitempty"`
	RunQuiet        string `json:"run_quiet,omitempty"`
	RungAnswer      string `json:"rung_answer,omitempty"`
	NoPushSessions  int    `json:"no_push_sessions,omitempty"`
}

// strings are j's bounds in the order of Waits.bounds.
func (j *waitsJSON) strings() [7]*string {
	return [7]*string{&j.Queued, &j.Dependency, &j.Gate, &j.MergeRetry, &j.ProviderBackoff, &j.RunQuiet, &j.RungAnswer}
}

// MarshalJSON is Waits as waitsJSON, its bounds strings, as Limits'.
func (w Waits) MarshalJSON() ([]byte, error) {
	j := waitsJSON{NoPushSessions: w.NoPushSessions}
	s := j.strings()
	for i, b := range w.bounds() {
		if *b.d != 0 {
			*s[i] = b.d.String()
		}
	}
	return json.Marshal(j)
}

// UnmarshalJSON reads waitsJSON back into Waits. A bad duration fails the
// whole read.
func (w *Waits) UnmarshalJSON(b []byte) error {
	var j waitsJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	out := Waits{NoPushSessions: j.NoPushSessions}
	s := j.strings()
	for i, bound := range out.bounds() {
		if *s[i] == "" {
			continue
		}
		v, err := time.ParseDuration(*s[i])
		if err != nil {
			return fmt.Errorf("waits.%s: %w", bound.key, err)
		}
		*bound.d = v
	}
	*w = out
	return nil
}
