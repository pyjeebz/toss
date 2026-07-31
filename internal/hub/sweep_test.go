package hub

import (
	"testing"
	"time"
)

func expiring(id string, expiresAt time.Time) Item {
	return Item{ID: id, Content: "x", Origin: "test", CreatedAt: expiresAt.Add(-ItemTTL), ExpiresAt: expiresAt}
}

func TestSweepDropsExpiredItems(t *testing.T) {
	h := New()
	room, _ := h.Create()
	now := time.Now()

	room.Publish(expiring("01OLD", now.Add(-time.Minute)))
	room.Publish(expiring("01LIVE", now.Add(time.Hour)))

	items, _ := h.Sweep(now)
	if items != 1 {
		t.Fatalf("swept %d items, want 1", items)
	}
	left := room.Since("")
	if len(left) != 1 || left[0].ID != "01LIVE" {
		t.Fatalf("wrong item survived: %+v", left)
	}
}

// An item dying should vanish from an open tab, not linger until reload.
func TestExpiredItemsAreBroadcast(t *testing.T) {
	h := New()
	room, _ := h.Create()
	now := time.Now()
	room.Publish(expiring("01OLD", now.Add(-time.Minute)))

	sub := room.Subscribe()
	defer sub.Close()

	h.Sweep(now)

	select {
	case ev := <-sub.C():
		if ev.Kind != EventDeleted || ev.ID != "01OLD" {
			t.Fatalf("got %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("expiry was not announced to subscribers")
	}
}

// Since() must not hand out an expired item in the up-to-60s gap between sweeps.
func TestSinceHidesExpiredBeforeTheSweeperRuns(t *testing.T) {
	h := New()
	room, _ := h.Create()
	room.Publish(expiring("01OLD", time.Now().Add(-time.Second)))
	room.Publish(expiring("01LIVE", time.Now().Add(time.Hour)))

	got := room.Since("")
	if len(got) != 1 || got[0].ID != "01LIVE" {
		t.Fatalf("expired item served before sweep: %+v", got)
	}
}

func TestSweepDropsIdleRooms(t *testing.T) {
	h := New()
	room, _ := h.Create()
	now := time.Now()

	if _, dropped := h.Sweep(now); dropped != 0 {
		t.Fatal("a fresh room was collected")
	}

	// Idle past the cutoff.
	future := now.Add(RoomIdleTTL + time.Minute)
	if _, dropped := h.Sweep(future); dropped != 1 {
		t.Fatalf("idle room not collected")
	}
	if h.Get(room.ID) != nil {
		t.Fatal("room still resolvable after being dropped")
	}
}

// A tab left open for days must not have its room collected underneath it. A
// live stream does not refresh lastSeen, so the subscriber count is the guard.
func TestRoomWithASubscriberSurvives(t *testing.T) {
	h := New()
	room, _ := h.Create()
	sub := room.Subscribe()
	defer sub.Close()

	if _, dropped := h.Sweep(time.Now().Add(RoomIdleTTL + time.Hour)); dropped != 0 {
		t.Fatal("collected a room that someone was listening to")
	}
	if h.Get(room.ID) == nil {
		t.Fatal("room vanished from under a subscriber")
	}
}

func TestTouchKeepsARoomAlive(t *testing.T) {
	h := New()
	room, _ := h.Create()

	room.Touch()
	if _, dropped := h.Sweep(time.Now().Add(time.Hour)); dropped != 0 {
		t.Fatal("recently touched room collected")
	}
}

func TestSweepIsSafeOnAnEmptyHub(t *testing.T) {
	items, rooms := New().Sweep(time.Now())
	if items != 0 || rooms != 0 {
		t.Fatalf("got %d items %d rooms", items, rooms)
	}
}

// The sweeper runs while traffic is flowing; this is the one the race detector
// is here for.
func TestSweepConcurrentWithTraffic(t *testing.T) {
	h := New()
	room, _ := h.Create()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 400 {
			// Half of these are born already expired.
			exp := time.Now().Add(time.Hour)
			if i%2 == 0 {
				exp = time.Now().Add(-time.Hour)
			}
			room.Publish(expiring(string(rune('A'+i%26))+string(rune('a'+i%26)), exp))
		}
	}()

	for range 200 {
		h.Sweep(time.Now())
		room.Since("")
		sub := room.Subscribe()
		sub.Close()
	}
	<-done
}
