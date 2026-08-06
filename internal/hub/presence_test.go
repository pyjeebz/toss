package hub

import (
	"sync"
	"testing"
	"time"
)

// nextPresence returns the next presence count, skipping content events.
func nextPresence(t *testing.T, sub *Subscription) int {
	t.Helper()
	for {
		select {
		case ev := <-sub.C():
			if ev.Kind != EventPresence {
				continue
			}
			return ev.Count
		case <-time.After(time.Second):
			t.Fatal("no presence event")
			return 0
		}
	}
}

// A device has no other way to find out whether anything is listening, so the
// count has to arrive without being asked for.
func TestSubscribingLearnsTheCountImmediately(t *testing.T) {
	h := New()
	room, _ := h.Create()

	first := room.Subscribe()
	defer first.Close()

	if n := nextPresence(t, first); n != 1 {
		t.Fatalf("first subscriber saw count %d, want 1", n)
	}
}

func TestPresenceRisesAndFallsForEveryone(t *testing.T) {
	h := New()
	room, _ := h.Create()

	first := room.Subscribe()
	defer first.Close()
	if n := nextPresence(t, first); n != 1 {
		t.Fatalf("count %d after one subscriber, want 1", n)
	}

	second := room.Subscribe()
	// Both learn about the arrival: the one that was already here, and the one
	// that just turned up.
	if n := nextPresence(t, first); n != 2 {
		t.Errorf("existing subscriber saw %d on arrival, want 2", n)
	}
	if n := nextPresence(t, second); n != 2 {
		t.Errorf("arriving subscriber saw %d, want 2", n)
	}

	second.Close()
	if n := nextPresence(t, first); n != 1 {
		t.Errorf("survivor saw %d after a departure, want 1", n)
	}
}

// Close is documented as safe to call more than once, and that has to stay true
// now that it broadcasts: a second Close must not tell the room someone left
// again.
func TestClosingTwiceAnnouncesOnce(t *testing.T) {
	h := New()
	room, _ := h.Create()

	watcher := room.Subscribe()
	defer watcher.Close()
	if n := nextPresence(t, watcher); n != 1 {
		t.Fatalf("count %d, want 1", n)
	}

	other := room.Subscribe()
	if n := nextPresence(t, watcher); n != 2 {
		t.Fatalf("count %d, want 2", n)
	}

	other.Close()
	other.Close()

	if n := nextPresence(t, watcher); n != 1 {
		t.Fatalf("count %d after the first close, want 1", n)
	}
	select {
	case ev := <-watcher.C():
		t.Fatalf("the second Close announced something: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// Presence counts must never arrive out of order.
//
// The count is taken and delivered under one lock. Take it and send it in two
// steps instead, and two devices arriving at once can interleave -- take 5,
// take 6, send 6, send 5 -- leaving a subscriber that saw 6 sitting on 5.
// Presence carries no ID and is never replayed, so nothing later corrects it.
//
// The assertion is monotonicity rather than the final value. Checking only
// where it ends up passes with the broadcast outside the lock, because the
// last write usually does carry the highest count; the damage is transient
// and permanent at the same time, which is exactly what makes it worth
// pinning. A dropped event skips a number without going backwards, so this
// stays robust to a full buffer.
func TestPresenceCountsNeverGoBackwardsWhileDevicesArrive(t *testing.T) {
	const joiners = 96

	h := New()
	room, _ := h.Create()

	watcher := room.Subscribe()
	defer watcher.Close()

	var mu sync.Mutex
	var seen []int
	stop := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			select {
			case ev := <-watcher.C():
				if ev.Kind == EventPresence {
					mu.Lock()
					seen = append(seen, ev.Count)
					mu.Unlock()
				}
			case <-stop:
				return
			}
		}
	}()

	var wg sync.WaitGroup
	subs := make([]*Subscription, joiners)
	for i := range joiners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			subs[i] = room.Subscribe()
		}()
	}
	wg.Wait()

	time.Sleep(100 * time.Millisecond)
	close(stop)
	<-drained

	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(seen); i++ {
		if seen[i] < seen[i-1] {
			t.Fatalf("count went backwards at %d: %v", i, seen[max(0, i-5):min(len(seen), i+2)])
		}
	}
	if n := len(seen); n == 0 || seen[n-1] != joiners+1 {
		t.Errorf("ended on %v, want %d", seen, joiners+1)
	}

	for _, s := range subs {
		s.Close()
	}
}
