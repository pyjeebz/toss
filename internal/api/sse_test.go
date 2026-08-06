package api

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/pyjeebz/toss/internal/hub"
)

// These cover the stream behaviours CLAUDE.md lists as load-bearing. Every one
// of them fails silently: the stream stays open, the status stays Live, and
// what goes missing is an item nobody knows to look for.

type stream struct {
	res   *http.Response
	lines chan string
}

func openStream(t *testing.T, url string, header map[string]string) *stream {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	s := &stream{res: res, lines: make(chan string, 256)}
	go func() {
		sc := bufio.NewScanner(res.Body)
		// Items in the ordering test are padded to widen the replay window, and
		// a padded item's data line is far past the scanner's 64 KB default.
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			s.lines <- sc.Text()
		}
		close(s.lines)
	}()
	return s
}

// next returns the next line matching pred, or fails.
func (s *stream) next(t *testing.T, what string, pred func(string) bool) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case line, ok := <-s.lines:
			if !ok {
				t.Fatalf("stream closed before %s", what)
			}
			if pred(line) {
				return line
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
			return ""
		}
	}
}

func (s *stream) nextItemID(t *testing.T) string {
	t.Helper()
	line := s.next(t, "an item id", func(l string) bool { return strings.HasPrefix(l, "id: ") })
	id, _ := strings.CutPrefix(line, "id: ")
	return id
}

func (s *stream) close() { s.res.Body.Close() }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func streamServer(t *testing.T) (*Server, *hub.Room, *httptest.Server) {
	t.Helper()
	s := newTestServer(t)
	room, err := s.Hub.Create()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Routes(stubStatic()))
	t.Cleanup(ts.Close)
	return s, room, ts
}

func publish(room *hub.Room, content string) hub.Item {
	it := hub.Item{
		ID:        ulid.Make().String(),
		IV:        "aXY",
		Content:   content,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(hub.ItemTTL),
	}
	room.Publish(it)
	return it
}

// A disconnected client must free its subscriber. Without the ctx.Done() case
// in the select, every reconnect leaks a goroutine and a map entry -- and a
// phone reconnects all day, so this is the leak that eventually matters. It is
// invisible until the process is, which is why it gets a test rather than trust.
func TestDisconnectingFreesTheSubscriber(t *testing.T) {
	_, room, ts := streamServer(t)

	s := openStream(t, ts.URL+"/api/rooms/"+room.ID+"/stream", nil)
	waitFor(t, "the subscription to land", func() bool { return room.Subscribers() == 1 })

	s.close()
	waitFor(t, "the subscriber to be released", func() bool { return room.Subscribers() == 0 })
}

// The retry hint has to be the first thing on the wire. EventSource's default
// reconnect delay is browser-specific and can be several seconds; a phone
// waking up should be back before that.
func TestStreamOpensWithTheRetryHint(t *testing.T) {
	_, room, ts := streamServer(t)

	s := openStream(t, ts.URL+"/api/rooms/"+room.ID+"/stream", nil)
	defer s.close()

	first := s.next(t, "the first non-blank line", func(l string) bool { return l != "" })
	if first != "retry: 2000" {
		t.Errorf("stream opened with %q, want %q", first, "retry: 2000")
	}
}

// EventSource cannot set a Last-Event-ID header on its first connect, so the
// resume point has to be accepted from the query string as well. This path is
// only exercised on a first connect after a page load, which is exactly when
// nobody is watching.
func TestResumePointIsAcceptedFromTheQueryString(t *testing.T) {
	_, room, ts := streamServer(t)

	first := publish(room, "one")
	publish(room, "two")

	s := openStream(t, ts.URL+"/api/rooms/"+room.ID+"/stream?last_event_id="+first.ID, nil)
	defer s.close()

	if got := s.nextItemID(t); got == first.ID {
		t.Fatal("the item at the resume point was replayed; since is meant to be exclusive")
	}
}

// And when both are present the header wins, because it is the live one: the
// query string is a stale value frozen at page load.
func TestTheHeaderBeatsTheQueryString(t *testing.T) {
	_, room, ts := streamServer(t)

	one := publish(room, "one")
	two := publish(room, "two")
	three := publish(room, "three")

	// Query says resume after one; header says resume after two. Only three
	// should arrive.
	s := openStream(t, ts.URL+"/api/rooms/"+room.ID+"/stream?last_event_id="+one.ID,
		map[string]string{"Last-Event-ID": two.ID})
	defer s.close()

	if got := s.nextItemID(t); got != three.ID {
		t.Errorf("resumed at %q, want %q -- the query string overrode the header", got, three.ID)
	}
}

// Nothing published while the backlog is being replayed may be lost. Subscribe
// happens before the replay for exactly this reason; the other order drops
// whatever lands in between, and the overlap it creates instead is harmless
// because clients dedupe by ID.
//
// Only the items published *during* are asserted on. Two reasons. The backlog
// is covered by the resume tests, and more importantly the room caps at
// MaxItems, so a test that demanded every backlog item back would fail on
// trimming rather than on ordering.
//
// The backlog is still published, and deliberately near the cap: replaying it
// is what makes the mutated window wide enough to fall into. The publish rate
// during is deliberately slow, because fan-out drops for a consumer that is
// behind, so flooding would fail for a reason that is by design.
//
// Racy by nature, so it was mutation-checked rather than assumed. Swapping the
// subscribe and the replay in sse.go fails this 8 times in 8; the correct
// ordering passes 15 in 15 under -race. Without the padding it was 3 in 8,
// which is the difference between a guard and a coin.
func TestNothingIsLostBetweenSubscribingAndReplaying(t *testing.T) {
	const backlog = 49 // just under hub.MaxItems
	const during = 30

	_, room, ts := streamServer(t)

	want := make(map[string]bool, during)
	// Padded, so replaying the backlog takes long enough to be a window worth
	// falling into. With small items the mutated ordering is only wrong for a
	// millisecond or two and mostly gets away with it.
	pad := strings.Repeat("x", 8<<10)
	for i := range backlog {
		publish(room, fmt.Sprintf("backlog-%d-%s", i, pad))
	}

	// Publish continuously while the stream is being established, so something
	// is always in flight during the replay.
	stop := make(chan struct{})
	published := make(chan string, during)
	go func() {
		defer close(published)
		for i := range during {
			select {
			case <-stop:
				return
			default:
			}
			published <- publish(room, fmt.Sprintf("during-%d", i)).ID
			time.Sleep(time.Millisecond)
		}
	}()

	s := openStream(t, ts.URL+"/api/rooms/"+room.ID+"/stream", nil)
	defer s.close()

	for id := range published {
		want[id] = true
	}
	close(stop)

	// Count what is still outstanding rather than comparing sizes. The stream
	// also carries the backlog, so `len(got) < len(want)` would be satisfied by
	// replayed items alone and the loop would exit having checked nothing.
	got := make(map[string]bool, len(want))
	outstanding := len(want)
	deadline := time.After(5 * time.Second)
	for outstanding > 0 {
		select {
		case line, ok := <-s.lines:
			if !ok {
				t.Fatal("stream closed early")
			}
			id, found := strings.CutPrefix(line, "id: ")
			if found && want[id] && !got[id] {
				got[id] = true
				outstanding--
			}
		case <-deadline:
			var missing []string
			for id := range want {
				if !got[id] {
					missing = append(missing, id)
				}
			}
			t.Fatalf("%d of %d items published while the stream was opening never arrived, e.g. %v",
				len(missing), len(want), missing[:min(3, len(missing))])
		}
	}
}

// A deleted event must not carry an id:.
//
// The failure is not that deletions stop working -- they do not. It is that
// EventSource sets Last-Event-ID from any id: it sees, so a client that has not
// yet processed everything published before the deletion resumes from the
// deleted item's ULID and Since() skips whatever sat between. Those items are
// gone for that device, with no error and nothing to notice.
func TestDeletedEventCarriesNoEventID(t *testing.T) {
	var sb strings.Builder
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder(), body: &sb}
	if err := writeDeleted(rec, "01ARZ3NDEKTSV4RRFFQ69G5FAV"); err != nil {
		t.Fatal(err)
	}
	got := sb.String()
	if strings.Contains(got, "id:") {
		t.Fatalf("deleted carries an id, which would poison Last-Event-ID:\n%s", got)
	}
	if !strings.HasPrefix(got, "event: deleted\n") {
		t.Fatalf("unexpected framing:\n%s", got)
	}
}

// Items are the only events that may carry an id:, because they are the only
// ones Since() can resume from.
func TestOnlyItemsCarryAnEventID(t *testing.T) {
	_, room, ts := streamServer(t)

	s := openStream(t, ts.URL+"/api/rooms/"+room.ID+"/stream", nil)
	defer s.close()
	s.next(t, "the retry hint", func(l string) bool { return l == "retry: 2000" })

	it := publish(room, "one")
	room.Delete(it.ID)
	room.Paired("iPhone · Safari")

	// Frames are blank-line separated and writeItem puts id: *before* event:,
	// so this has to be read a frame at a time. Tracking a running "current
	// event" instead attributes an item's id to whatever came before it.
	var frame []string
	seen := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for !seen["paired"] {
		select {
		case line, ok := <-s.lines:
			if !ok {
				t.Fatal("stream closed early")
			}
			if line != "" {
				frame = append(frame, line)
				continue
			}
			checkFrame(t, frame, seen)
			frame = nil
		case <-deadline:
			t.Fatalf("did not see every event kind; saw %v", seen)
		}
	}
}

// checkFrame asserts that a frame carries an id: if and only if it is an item.
func checkFrame(t *testing.T, frame []string, seen map[string]bool) {
	t.Helper()
	var kind string
	hasID := false
	for _, l := range frame {
		if k, ok := strings.CutPrefix(l, "event: "); ok {
			kind = k
		}
		if strings.HasPrefix(l, "id: ") {
			hasID = true
		}
	}
	if kind == "" {
		return // the retry hint, or a ping comment
	}
	seen[kind] = true

	if hasID && kind != "item" {
		t.Errorf("a %q frame carries an id:, which would poison Last-Event-ID:\n%s",
			kind, strings.Join(frame, "\n"))
	}
	if !hasID && kind == "item" {
		t.Errorf("an item frame carries no id:, so Last-Event-ID can never advance:\n%s",
			strings.Join(frame, "\n"))
	}
}

// GET /r/{room} deliberately does not check the room exists.
//
// This looks like a missing 404 and is not. The key lives in the URL fragment,
// which never reaches the server, so the client has to be running before it can
// tell whether it can read the room at all -- and a stale room is something it
// recovers from by minting a new one. Serving the page is what lets it get far
// enough to find out.
func TestRoomPageDoesNotCheckTheRoomExists(t *testing.T) {
	s := newTestServer(t)
	h := s.Routes(stubStatic())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/nosuchroomatall", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status %d for an unknown room, want 200 -- see the comment above this test", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want html", ct)
	}
}

// The stream for a room that does not exist is a 404, unlike the page. The
// client uses this to tell "my room is gone" from "my network blipped", and
// onStreamError depends on the difference.
func TestStreamingAnUnknownRoomIs404(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.Routes(stubStatic()))
	defer ts.Close()

	res, err := http.Get(ts.URL + "/api/rooms/nosuchroomatall/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)

	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", res.StatusCode)
	}
}
