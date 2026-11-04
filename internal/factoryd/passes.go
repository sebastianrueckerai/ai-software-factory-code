package factoryd

import (
	"context"
	"log"
	"sync"
	"time"
)

// Passes runs factoryd's periodic passes, each on its own goroutine under a
// bound, so a pass that hangs holds none of the others. It keeps what
// /readyz and factory.pass.late read: each pass's newest end, and whether
// its last run passed its bound. A pass that has not ended yet ages from
// its start.
type Passes struct {
	mu    sync.Mutex
	ended map[string]time.Time
	late  map[string]bool
}

// Run starts pass name on its own goroutine (passLoop) until ctx ends.
func (p *Passes) Run(ctx context.Context, name string, every, bound time.Duration, fn func(context.Context) error) {
	p.mark(name, time.Now(), false)
	go p.passLoop(ctx, name, every, bound, fn)
}

// Ages is each pass's time since its newest end at now.
func (p *Passes) Ages(now time.Time) map[string]time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	ages := make(map[string]time.Duration, len(p.ended))
	for name, at := range p.ended {
		ages[name] = now.Sub(at)
	}
	return ages
}

// passLoop runs fn at once, then every `every`, each run under a context
// bounded by bound. A run past its bound is logged with its name and the
// time over, sets the gauge factory.pass.late{pass} to 1 until a run ends
// within the bound, and is cancelled by the context. A run never overlaps
// the one before it: one that ignores its context holds its own loop, and
// the watchdog shows it late at the bound.
func (p *Passes) passLoop(ctx context.Context, name string, every, bound time.Duration, fn func(context.Context) error) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		p.once(ctx, name, bound, fn)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (p *Passes) once(ctx context.Context, name string, bound time.Duration, fn func(context.Context) error) {
	start := time.Now()
	run, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fn(run) }()
	var err error
	select {
	case err = <-done:
	case <-run.Done():
		if run.Err() == context.DeadlineExceeded {
			p.setLate(name)
			log.Printf("pass %s: still running at its bound of %s, so its context is cancelled", name, durationText(bound))
		}
		err = <-done
	}
	end := time.Now()
	if err != nil && ctx.Err() == nil {
		log.Printf("pass %s: %v", name, err)
	}
	took := end.Sub(start)
	late := took > bound || run.Err() == context.DeadlineExceeded
	if late {
		log.Printf("pass %s: ended %s over its bound of %s", name, (took - bound).Round(time.Millisecond), durationText(bound))
	}
	p.mark(name, end, late)
}

func (p *Passes) mark(name string, at time.Time, late bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended == nil {
		p.ended, p.late = map[string]time.Time{}, map[string]bool{}
	}
	p.ended[name], p.late[name] = at, late
}

func (p *Passes) setLate(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.late[name] = true
}
