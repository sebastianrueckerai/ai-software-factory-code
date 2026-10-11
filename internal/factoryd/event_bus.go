package factoryd

import (
	"sync"
)

// EventBus is the in-process broadcaster: Events.Record fans each row out
// to the subscribers of its project. One replica is the assumption; a
// later multi-replica design would replace this with a pubsub bus.
type EventBus struct {
	mu   sync.RWMutex
	subs map[string]map[chan Event]struct{} // by repository
}

// NewEventBus builds an empty bus.
func NewEventBus() *EventBus {
	return &EventBus{subs: map[string]map[chan Event]struct{}{}}
}

// Subscribe registers ch for repository's events and returns a cancel
// function that removes it. The channel is closed when the subscriber is
// removed, so a range loop ends.
func (b *EventBus) Subscribe(repository string, ch chan Event) func() {
	b.mu.Lock()
	set := b.subs[repository]
	if set == nil {
		set = map[chan Event]struct{}{}
		b.subs[repository] = set
	}
	set[ch] = struct{}{}
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		if set, ok := b.subs[repository]; ok {
			if _, live := set[ch]; live {
				delete(set, ch)
				close(ch)
			}
		}
		b.mu.Unlock()
	}
}

// Publish sends e to every subscriber of e.Repository. A slow subscriber
// blocks the others; one replica keeps the fan-out cheap.
func (b *EventBus) Publish(e Event) {
	b.mu.RLock()
	set := b.subs[e.Repository]
	b.mu.RUnlock()
	for ch := range set {
		ch <- e
	}
}
