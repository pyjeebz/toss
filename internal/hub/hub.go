// Package hub holds every room, every item and every live subscriber, in
// memory, in one process. There is no database and no cross-process fan-out.
// See the comment in cmd/toss/main.go before you consider scaling this out.
package hub

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"log/slog"
	"sync"
	"time"
)

const (
	// MaxItems is the per-room retention cap. Oldest items are dropped first.
	MaxItems = 50

	// ItemTTL is how long an item survives before it expires.
	ItemTTL = 24 * time.Hour

	// RoomIdleTTL is how long a room with nobody listening survives.
	RoomIdleTTL = 48 * time.Hour

	// SweepInterval is how often expired items and idle rooms are collected.
	SweepInterval = 60 * time.Second

	// subBuffer is the per-subscriber channel depth. A client that overruns it
	// drops events and recovers via backfill; blocking the publisher instead
	// would let one stalled phone stall the whole room.
	subBuffer = 16

	// roomIDBytes is 120 bits of crypto/rand -- unguessable, unlike the short
	// pairing code that will resolve to it in M1.
	roomIDBytes = 15
)

// EventKind distinguishes the SSE events a subscriber can receive.
type EventKind string

const (
	EventItem     EventKind = "item"
	EventDeleted  EventKind = "deleted"
	EventPaired   EventKind = "paired"
	EventPresence EventKind = "presence"
)

// Event is one thing that happened in a room.
//
// The brief's subscriber carries a bare Item; it carries an Event instead so
// that deletions can ride the same channel as arrivals without encoding "this
// one is a tombstone" into the Item itself. Item is only meaningful for
// EventItem, Origin only for EventPaired, Count only for EventPresence.
type Event struct {
	Kind   EventKind
	ID     string
	Item   Item
	Origin string
	Count  int
}

type subscriber struct {
	ch chan Event
}

// Room is one pairing of devices and the items they have tossed at each other.
type Room struct {
	mu       sync.RWMutex
	ID       string
	items    []Item // newest last, cap MaxItems, drop from front
	subs     map[*subscriber]struct{}
	lastSeen time.Time // read by the sweeper in M2
}

// Hub is the set of live rooms.
type Hub struct {
	mu    sync.RWMutex
	rooms map[string]*Room
}

// New returns an empty hub.
func New() *Hub {
	return &Hub{rooms: make(map[string]*Room)}
}

// Create mints a room with an unguessable ID and registers it.
func (h *Hub) Create() (*Room, error) {
	id, err := newRoomID()
	if err != nil {
		return nil, err
	}
	r := &Room{
		ID:       id,
		subs:     make(map[*subscriber]struct{}),
		lastSeen: time.Now(),
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rooms[id] = r
	return r, nil
}

// Get returns the room with this ID, or nil. Rooms are never created
// implicitly: an unknown ID is a 404, not a new room.
func (h *Hub) Get(id string) *Room {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.rooms[id]
}

// Len reports how many rooms are live. Used by tests and /healthz.
func (h *Hub) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.rooms)
}

func newRoomID() (string, error) {
	b := make([]byte, roomIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	return lower(enc), nil
}

func lower(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'A' && c <= 'Z' {
			out[i] = c + ('a' - 'A')
		}
	}
	return string(out)
}

// Publish appends an item and fans it out to every live subscriber.
func (r *Room) Publish(it Item) {
	r.mu.Lock()
	r.items = append(r.items, it)
	if len(r.items) > MaxItems {
		trimmed := make([]Item, MaxItems)
		copy(trimmed, r.items[len(r.items)-MaxItems:])
		r.items = trimmed
	}
	r.lastSeen = time.Now()
	r.mu.Unlock()

	r.broadcast(Event{Kind: EventItem, ID: it.ID, Item: it})
}

// Paired announces that a device has joined, so the device that offered the
// code can stop showing it and say so.
//
// This is the one event the server originates rather than relays, and it is
// pure notification: nothing is stored, nothing is replayed, and origin is the
// same cosmetic "Windows · Chrome" label items carry. It touches no content, so
// it costs the end-to-end guarantee nothing.
func (r *Room) Paired(origin string) {
	r.Touch()
	r.broadcast(Event{Kind: EventPaired, Origin: origin})
}

// Delete removes one item and reports whether it was there.
func (r *Room) Delete(id string) bool {
	r.mu.Lock()
	found := false
	for i, it := range r.items {
		if it.ID == id {
			r.items = append(r.items[:i], r.items[i+1:]...)
			found = true
			break
		}
	}
	r.lastSeen = time.Now()
	r.mu.Unlock()

	if found {
		r.broadcast(Event{Kind: EventDeleted, ID: id})
	}
	return found
}

// Clear drops every item, emitting one deletion per item so that subscribers
// converge without needing a third event kind.
func (r *Room) Clear() int {
	r.mu.Lock()
	ids := make([]string, len(r.items))
	for i, it := range r.items {
		ids[i] = it.ID
	}
	r.items = nil
	r.lastSeen = time.Now()
	r.mu.Unlock()

	for _, id := range ids {
		r.broadcast(Event{Kind: EventDeleted, ID: id})
	}
	return len(ids)
}

// Since returns items whose ULID sorts after the given one, oldest first. An
// empty since returns everything the room holds. ULIDs are lexicographically
// ordered by time, so a string compare is the whole implementation.
//
// Expired items are filtered here as well as collected by the sweeper: the
// sweeper runs once a minute, and nothing should be handed out in the gap.
func (r *Room) Since(since string) []Item {
	now := time.Now()

	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Item, 0, len(r.items))
	for _, it := range r.items {
		if now.After(it.ExpiresAt) {
			continue
		}
		if since == "" || it.ID > since {
			out = append(out, it)
		}
	}
	return out
}

// IDs returns the ID of every unexpired item, oldest first.
//
// Separate from Since() rather than derived from it, because the caller only
// wants to know what still exists and Since() copies the items to say so. A
// full room is 50 items of up to 256 KB; this answer is fifty ULIDs.
//
// Expired items are filtered here for the same reason Since() filters them: the
// sweeper runs once a minute and nothing should be reported alive in the gap.
func (r *Room) IDs() []string {
	now := time.Now()

	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]string, 0, len(r.items))
	for _, it := range r.items {
		if now.After(it.ExpiresAt) {
			continue
		}
		out = append(out, it.ID)
	}
	return out
}

// StartSweeper collects expired items and idle rooms until ctx is cancelled.
// Runs in its own goroutine; call it once, from main.
func (h *Hub) StartSweeper(ctx context.Context, log *slog.Logger) {
	go func() {
		t := time.NewTicker(SweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				items, rooms := h.Sweep(now)
				if items > 0 || rooms > 0 {
					log.Info("swept", "items", items, "rooms", rooms, "remaining", h.Len())
				}
			}
		}
	}()
}

// Sweep drops expired items and idle rooms, returning how many of each went.
// Exported and taking an explicit now so it can be tested without waiting.
func (h *Hub) Sweep(now time.Time) (expiredItems, droppedRooms int) {
	// Snapshot first. Item expiry takes each room's lock in turn, and holding
	// the hub lock across all of that would stall every request in the process.
	h.mu.RLock()
	rooms := make([]*Room, 0, len(h.rooms))
	for _, r := range h.rooms {
		rooms = append(rooms, r)
	}
	h.mu.RUnlock()

	for _, r := range rooms {
		expiredItems += r.dropExpired(now)
	}

	// Re-check idleness while holding the hub lock. A device can subscribe
	// between the two phases, and dropping its room out from under it would
	// leave it streaming into a room that no longer exists.
	h.mu.Lock()
	for id, r := range h.rooms {
		if r.idle(now) {
			delete(h.rooms, id)
			droppedRooms++
		}
	}
	h.mu.Unlock()

	return expiredItems, droppedRooms
}

// dropExpired removes items past their TTL and tells subscribers, so an item
// vanishes from an open tab at the moment it dies rather than on next load.
func (r *Room) dropExpired(now time.Time) int {
	r.mu.Lock()
	kept := r.items[:0]
	var gone []string
	for _, it := range r.items {
		if now.After(it.ExpiresAt) {
			gone = append(gone, it.ID)
			continue
		}
		kept = append(kept, it)
	}
	// Release the tail so the dropped items are not pinned by the backing array.
	for i := len(kept); i < len(r.items); i++ {
		r.items[i] = Item{}
	}
	r.items = kept
	r.mu.Unlock()

	for _, id := range gone {
		r.broadcast(Event{Kind: EventDeleted, ID: id})
	}
	return len(gone)
}

// idle reports whether a room can be collected: nobody listening, and nothing
// has touched it in RoomIdleTTL.
//
// The subscriber check is what protects a tab left open for days. A live stream
// does not refresh lastSeen on its own, so without this a quiet room with a
// reader in it would be collected out from under them.
func (r *Room) idle(now time.Time) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.subs) == 0 && now.Sub(r.lastSeen) > RoomIdleTTL
}

// Touch marks the room as recently used so the sweeper leaves it alone.
func (r *Room) Touch() {
	r.mu.Lock()
	r.lastSeen = time.Now()
	r.mu.Unlock()
}

// Subscription is a live listener on a room. Close it in a defer, always.
type Subscription struct {
	room *Room
	sub  *subscriber
}

// C is the stream of events. It is never closed; the reader leaves by way of
// its request context.
func (s *Subscription) C() <-chan Event { return s.sub.ch }

// Close unsubscribes. Safe to call more than once.
func (s *Subscription) Close() {
	s.room.mu.Lock()
	_, was := s.room.subs[s.sub]
	delete(s.room.subs, s.sub)
	if was {
		// Under the lock, for the reason given in Subscribe.
		s.room.broadcastLocked(Event{Kind: EventPresence, Count: len(s.room.subs)})
	}
	s.room.mu.Unlock()
}

// Subscribe registers a listener. Subscribe BEFORE replaying backlog: doing it
// the other way round drops anything published in between. The overlap can
// deliver an item twice, which clients dedupe by ID -- losing one, they cannot.
func (r *Room) Subscribe() *Subscription {
	s := &subscriber{ch: make(chan Event, subBuffer)}
	r.mu.Lock()
	r.subs[s] = struct{}{}
	r.lastSeen = time.Now()
	// Announced from inside the lock, unlike every other broadcast. Taking the
	// count and then fanning it out in two steps lets two devices subscribing at
	// once interleave, and a subscriber that receives count=2 before count=1
	// settles on the wrong number and stays there -- presence carries no ID and
	// is never replayed, so nothing later corrects it. Serialising the count
	// with its own delivery is what makes the last event the true one.
	//
	// Safe to send while holding the lock only because the send is
	// non-blocking; see broadcastLocked. The new subscriber is already in the
	// map, so it learns the count on connect without a separate first message.
	r.broadcastLocked(Event{Kind: EventPresence, Count: len(r.subs)})
	r.mu.Unlock()
	return &Subscription{room: r, sub: s}
}

// Subscribers reports the live listener count. Used by tests.
func (r *Room) Subscribers() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.subs)
}

func (r *Room) broadcast(ev Event) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	r.broadcastLocked(ev)
}

// broadcastLocked fans out with r.mu already held, either side. Every send is
// non-blocking, so holding the lock across the loop cannot stall on a slow
// consumer -- which is what makes it safe to call from inside the write-locked
// sections of Subscribe and Close.
func (r *Room) broadcastLocked(ev Event) {
	for s := range r.subs {
		select {
		case s.ch <- ev:
		default: // slow consumer -- drop, don't block
		}
	}
}
