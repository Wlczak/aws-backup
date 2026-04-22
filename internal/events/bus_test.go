package events

import (
	"sync"
	"testing"
	"time"

	"github.com/Wlczak/aws-backup/internal/engine"
)

func TestBusFanout(t *testing.T) {
	bus := NewBus(16)
	a := bus.Subscribe()
	b := bus.Subscribe()
	defer a.Close()
	defer b.Close()

	if bus.SubscriberCount() != 2 {
		t.Fatalf("want 2 subscribers, got %d", bus.SubscriberCount())
	}

	ev := engine.Event{Type: engine.EventUploadComplete, RunID: 7}
	bus.Publish(ev)

	for _, s := range []*Subscription{a, b} {
		select {
		case got := <-s.Chan():
			if got.Type != ev.Type || got.RunID != ev.RunID {
				t.Errorf("subscriber got %+v, want %+v", got, ev)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for event")
		}
	}
}

func TestBusSlowSubscriberDropped(t *testing.T) {
	bus := NewBus(1)
	s := bus.Subscribe()
	defer s.Close()

	// Publish 5 events without reading — only 1 fits in the buffer,
	// the other 4 should be counted as dropped.
	for i := 0; i < 5; i++ {
		bus.Publish(engine.Event{Type: engine.EventScanProgress})
	}
	if got := bus.Dropped(); got != 4 {
		t.Errorf("dropped=%d want 4", got)
	}
}

func TestBusCloseUnsubscribes(t *testing.T) {
	bus := NewBus(4)
	s := bus.Subscribe()
	s.Close()
	if bus.SubscriberCount() != 0 {
		t.Errorf("want 0 subscribers after Close, got %d", bus.SubscriberCount())
	}
	// Double-close is safe.
	s.Close()
	// Publish after close should not panic.
	bus.Publish(engine.Event{Type: engine.EventRunComplete})
}

func TestBusConcurrent(t *testing.T) {
	bus := NewBus(100)
	s := bus.Subscribe()
	defer s.Close()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				bus.Publish(engine.Event{Type: engine.EventScanProgress})
			}
		}()
	}

	received := 0
	done := make(chan struct{})
	go func() {
		for range s.Chan() {
			received++
			if received == 100 {
				close(done)
				return
			}
		}
	}()
	wg.Wait()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("received %d events, want 100", received)
	}
}
