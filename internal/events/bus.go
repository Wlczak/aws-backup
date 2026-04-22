// Package events is a tiny in-process pub/sub fan-out for engine.Event.
// Publish never blocks: slow subscribers are dropped rather than stalling
// the engine mid-upload.
package events

import (
	"sync"
	"sync/atomic"

	"github.com/Wlczak/aws-backup/internal/engine"
)

// Bus fans out engine.Event values to any number of subscribers.
// The zero value is unusable; always obtain one via NewBus.
type Bus struct {
	mu          sync.RWMutex
	subscribers map[int64]*Subscription
	nextID      int64
	bufferSize  int

	// dropped counts events dropped because a subscriber's buffer was full.
	// Exposed for tests / telemetry.
	dropped atomic.Int64
}

// NewBus returns a Bus where each Subscription has a buffered channel of
// the given capacity.
func NewBus(bufferSize int) *Bus {
	if bufferSize <= 0 {
		bufferSize = 64
	}
	return &Bus{
		subscribers: map[int64]*Subscription{},
		bufferSize:  bufferSize,
	}
}

// Publish delivers ev to every subscriber non-blockingly. If a
// subscriber's buffer is full the event is dropped (bus.Dropped() is
// incremented) and publishing continues for the rest.
func (b *Bus) Publish(ev engine.Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, s := range b.subscribers {
		select {
		case s.c <- ev:
		default:
			b.dropped.Add(1)
		}
	}
}

// Subscribe returns a new Subscription. Caller MUST call Subscription.Close
// to release the channel; leaked subscriptions hold state in the bus.
func (b *Bus) Subscribe() *Subscription {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	s := &Subscription{
		id:  b.nextID,
		c:   make(chan engine.Event, b.bufferSize),
		bus: b,
	}
	b.subscribers[s.id] = s
	return s
}

// Dropped returns the cumulative count of events dropped across all slow
// subscribers.
func (b *Bus) Dropped() int64 { return b.dropped.Load() }

// SubscriberCount returns the number of live subscriptions.
func (b *Bus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// Subscription is a receive-only event stream attached to a Bus.
type Subscription struct {
	id     int64
	c      chan engine.Event
	bus    *Bus
	closed atomic.Bool
}

// Chan returns the receive-only channel of events.
func (s *Subscription) Chan() <-chan engine.Event { return s.c }

// Close removes the subscription from its Bus and closes the channel.
// Safe to call more than once.
func (s *Subscription) Close() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	s.bus.mu.Lock()
	delete(s.bus.subscribers, s.id)
	s.bus.mu.Unlock()
	close(s.c)
}
