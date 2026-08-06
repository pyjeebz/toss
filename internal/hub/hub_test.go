package hub

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func item(id string) Item {
	now := time.Now()
	return Item{ID: id, Content: "x", Origin: "test", CreatedAt: now, ExpiresAt: now.Add(ItemTTL)}
}

func TestPublishReachesSubscribers(t *testing.T) {
	h := New()
	room, err := h.Create()
	if err != nil {
		t.Fatal(err)
	}

	sub := room.Subscribe()
	defer sub.Close()

	room.Publish(item("01AAA"))

	if ev := nextContent(t, sub); ev.Kind != EventItem || ev.ID != "01AAA" {
		t.Fatalf("got %+v", ev)
	}
}

// nextContent returns the next event that says something happened to the room's
// contents, skipping presence.
//
// Subscribing emits a presence event, and so does every device arriving or
// leaving afterwards, so a test that reads the channel raw gets a count where
// it expected an item. Tests that are actually about presence are below and do
// not use this.
func nextContent(t *testing.T, sub *Subscription) Event {
	t.Helper()
	for {
		select {
		case ev := <-sub.C():
			if ev.Kind == EventPresence {
				continue
			}
			return ev
		case <-time.After(time.Second):
			t.Fatal("no event")
			return Event{}
		}
	}
}

func TestSlowConsumerDoesNotBlockPublisher(t *testing.T) {
	h := New()
	room, _ := h.Create()

	stalled := room.Subscribe() // never drained
	defer stalled.Close()
	healthy := room.Subscribe()
	defer healthy.Close()

	done := make(chan struct{})
	go func() {
		for i := range subBuffer * 4 {
			room.Publish(item(fmt.Sprintf("%05d", i)))
		}
		close(done)
	}()

	// Drain the healthy subscriber so it does not fill up too.
	go func() {
		for range healthy.C() {
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher blocked on a stalled subscriber")
	}
}

func TestItemsCappedAtMax(t *testing.T) {
	h := New()
	room, _ := h.Create()

	for i := range MaxItems + 20 {
		room.Publish(item(fmt.Sprintf("%05d", i)))
	}

	got := room.Since("")
	if len(got) != MaxItems {
		t.Fatalf("kept %d items, want %d", len(got), MaxItems)
	}
	// Oldest dropped from the front, newest retained.
	if got[0].ID != "00020" {
		t.Fatalf("oldest retained is %s, want 00020", got[0].ID)
	}
	if got[len(got)-1].ID != fmt.Sprintf("%05d", MaxItems+19) {
		t.Fatalf("newest retained is %s", got[len(got)-1].ID)
	}
}

func TestSinceIsExclusive(t *testing.T) {
	h := New()
	room, _ := h.Create()
	room.Publish(item("01A"))
	room.Publish(item("01B"))
	room.Publish(item("01C"))

	got := room.Since("01A")
	if len(got) != 2 || got[0].ID != "01B" || got[1].ID != "01C" {
		t.Fatalf("got %+v", got)
	}
}

func TestDeleteAndClearBroadcast(t *testing.T) {
	h := New()
	room, _ := h.Create()
	room.Publish(item("01A"))
	room.Publish(item("01B"))

	sub := room.Subscribe()
	defer sub.Close()

	if !room.Delete("01A") {
		t.Fatal("delete reported miss")
	}
	if room.Delete("01A") {
		t.Fatal("second delete reported hit")
	}
	if ev := nextContent(t, sub); ev.Kind != EventDeleted || ev.ID != "01A" {
		t.Fatalf("got %+v", ev)
	}

	if n := room.Clear(); n != 1 {
		t.Fatalf("cleared %d, want 1", n)
	}
	if ev := nextContent(t, sub); ev.Kind != EventDeleted || ev.ID != "01B" {
		t.Fatalf("got %+v", ev)
	}
	if len(room.Since("")) != 0 {
		t.Fatal("items survived clear")
	}
}

func TestUnknownRoomIsNil(t *testing.T) {
	if New().Get("nope") != nil {
		t.Fatal("rooms must never be created implicitly")
	}
}

func TestRoomIDsAreDistinctAndSized(t *testing.T) {
	h := New()
	seen := make(map[string]bool)
	for range 1000 {
		r, err := h.Create()
		if err != nil {
			t.Fatal(err)
		}
		if len(r.ID) != 24 { // 120 bits of base32
			t.Fatalf("room id %q is %d chars, want 24", r.ID, len(r.ID))
		}
		if seen[r.ID] {
			t.Fatalf("duplicate room id %q", r.ID)
		}
		seen[r.ID] = true
	}
}

// The one the race detector cares about: rooms mutated while subscribers join
// and leave underneath.
func TestConcurrentPublishSubscribe(t *testing.T) {
	h := New()
	room, _ := h.Create()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for w := range 8 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range 200 {
				room.Publish(item(fmt.Sprintf("%02d-%05d", w, i)))
			}
		}(w)
	}

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				sub := room.Subscribe()
				select {
				case <-sub.C():
				default:
				}
				sub.Close()
			}
		}()
	}

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				room.Since("")
				room.Subscribers()
				room.Touch()
			}
		}()
	}

	// Let the publishers finish, then wind the readers down.
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	if room.Subscribers() != 0 {
		t.Fatalf("%d subscribers leaked", room.Subscribers())
	}
}

func TestConcurrentRoomCreation(t *testing.T) {
	h := New()
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.Create(); err != nil {
				t.Error(err)
			}
			h.Get("whatever")
			h.Len()
		}()
	}
	wg.Wait()
	if h.Len() != 64 {
		t.Fatalf("hub holds %d rooms, want 64", h.Len())
	}
}
