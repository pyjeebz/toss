package hub

import (
	"slices"
	"testing"
	"time"
)

func TestIDsReportsEveryLiveItemOldestFirst(t *testing.T) {
	h := New()
	room, _ := h.Create()

	room.Publish(item("01A"))
	room.Publish(item("01B"))
	room.Publish(item("01C"))

	got := room.IDs()
	want := []string{"01A", "01B", "01C"}
	if !slices.Equal(got, want) {
		t.Errorf("IDs() = %v, want %v", got, want)
	}
}

// Same reason Since() filters: the sweeper runs once a minute, and an item that
// has expired must not be reported alive in the gap. A client reconciling
// against this would keep a dead scrap on screen.
func TestIDsHidesExpiredBeforeTheSweeperRuns(t *testing.T) {
	h := New()
	room, _ := h.Create()

	room.Publish(expiring("01OLD", time.Now().Add(-time.Minute)))
	room.Publish(item("01NEW"))

	got := room.IDs()
	if slices.Contains(got, "01OLD") {
		t.Errorf("IDs() = %v, want the expired item filtered out", got)
	}
	if !slices.Contains(got, "01NEW") {
		t.Errorf("IDs() = %v, want the live item present", got)
	}
}

func TestIDsOnAnEmptyRoomIsEmptyNotNil(t *testing.T) {
	h := New()
	room, _ := h.Create()

	got := room.IDs()
	if got == nil {
		t.Fatal("IDs() returned nil; it marshals to null rather than []")
	}
	if len(got) != 0 {
		t.Errorf("IDs() = %v, want empty", got)
	}
}

// The point of the endpoint is that it is small. Content is what makes a room
// heavy, and none of it should be anywhere near this answer.
func TestIDsDoesNotCarryContent(t *testing.T) {
	h := New()
	room, _ := h.Create()

	it := item("01A")
	it.Content = "some-distinctive-ciphertext"
	room.Publish(it)

	for _, id := range room.IDs() {
		if id != "01A" {
			t.Errorf("IDs() returned %q, want just the ID", id)
		}
	}
}
