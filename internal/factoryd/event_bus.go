package factoryd

import "sync"

// EventBus fans each recorded event out to the console streams of its
// project, in process: factoryd runs as one replica, so every Record and
// every stream meet in this one bus.
type EventBus struct {
	mu   sync.RWMutex
	subs map[string]map[chan Event]struct{} // by repository
}

// NewEventBus builds an empty bus.
func NewEventBus() *EventBus {
	return &EventBus{subs: map[string]map[chan Event]struct{}{}}
}

// Subscribe registers ch for repository's events. cancel removes it and
// closes ch, unless Publish dropped it first.
func (b *EventBus) Subscribe(repository string, ch chan Event) (cancel func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	set := b.subs[repository]
	if set == nil {
		set = map[chan Event]struct{}{}
		b.subs[repository] = set
	}
	set[ch] = struct{}{}
	return func() { b.drop(repository, ch) }
}

// drop removes ch from repository's subscribers and closes it, once.
func (b *EventBus) drop(repository string, ch chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	set := b.subs[repository]
	if _, ok := set[ch]; !ok {
		return
	}
	delete(set, ch)
	if len(set) == 0 {
		delete(b.subs, repository)
	}
	close(ch)
}

// Publish sends e to every subscriber of e.Repository without blocking
// Record. A subscriber whose buffer is full is dropped: its stream ends,
// the browser reconnects with Last-Event-ID, and the replay reads the
// rows it missed from the table.
func (b *EventBus) Publish(e Event) {
	var full []chan Event
	b.mu.RLock()
	for ch := range b.subs[e.Repository] {
		select {
		case ch <- e:
		default:
			full = append(full, ch)
		}
	}
	b.mu.RUnlock()
	for _, ch := range full {
		b.drop(e.Repository, ch)
	}
}
