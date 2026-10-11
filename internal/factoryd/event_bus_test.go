package factoryd

import (
	"sync"
	"testing"
)

// Publish races streams that subscribe and cancel, and its own drops of
// full subscribers: under -race the bus shows no data race, and no send or
// close meets a closed channel.
func TestEventBusPublishRacesSubscribeAndCancel(t *testing.T) {
	b := NewEventBus()
	done := make(chan struct{})
	var subscribers, publishers sync.WaitGroup
	for range 4 {
		subscribers.Add(1)
		go func() {
			defer subscribers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				ch := make(chan Event, 1)
				cancel := b.Subscribe("sf/r", ch)
				select {
				case <-ch:
				default:
				}
				cancel()
				cancel()
			}
		}()
	}
	for range 3 {
		publishers.Add(1)
		go func() {
			defer publishers.Done()
			for i := range 2000 {
				b.Publish(Event{ID: int64(i), Repository: "sf/r"})
			}
		}()
	}
	publishers.Wait()
	close(done)
	subscribers.Wait()
	if n := len(b.subs); n != 0 {
		t.Fatalf("%d repositories keep subscribers after every stream cancelled", n)
	}
}

// A subscriber whose buffer is full is dropped and its channel closed,
// without blocking Publish; the others of the project still get the event,
// another project's get nothing, and a cancel after the drop is a no-op.
func TestEventBusDropsAFullSubscriber(t *testing.T) {
	b := NewEventBus()
	full, roomy, other := make(chan Event, 1), make(chan Event, 2), make(chan Event, 1)
	cancelFull := b.Subscribe("sf/r", full)
	defer b.Subscribe("sf/r", roomy)()
	defer b.Subscribe("sf/other", other)()
	b.Publish(Event{ID: 1, Repository: "sf/r"})
	b.Publish(Event{ID: 2, Repository: "sf/r"})
	if e, ok := <-full; !ok || e.ID != 1 {
		t.Fatalf("the full subscriber's first read = %d, %v; want event 1", e.ID, ok)
	}
	if e, ok := <-full; ok {
		t.Fatalf("the full subscriber got event %d; want its channel closed", e.ID)
	}
	cancelFull()
	for _, want := range []int64{1, 2} {
		if e := <-roomy; e.ID != want {
			t.Fatalf("the subscriber with room got event %d, want %d", e.ID, want)
		}
	}
	select {
	case e := <-other:
		t.Fatalf("another project's subscriber got event %d", e.ID)
	default:
	}
}
