package api

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A device connecting has to be told the count without asking. The whole
// question the feature answers -- "is my laptop still listening?" -- has no
// answer on a stream that only speaks when an item arrives.
func TestStreamReportsPresenceOnConnectAndOnChange(t *testing.T) {
	s := newTestServer(t)
	room, err := s.Hub.Create()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Routes(stubStatic()))
	defer ts.Close()

	open := func() (*http.Response, chan string) {
		t.Helper()
		res, err := http.Get(ts.URL + "/api/rooms/" + room.ID + "/stream")
		if err != nil {
			t.Fatal(err)
		}
		lines := make(chan string, 32)
		go func() {
			sc := bufio.NewScanner(res.Body)
			for sc.Scan() {
				lines <- sc.Text()
			}
		}()
		return res, lines
	}

	// nextCount reads until a presence payload turns up.
	nextCount := func(lines chan string) string {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case line := <-lines:
				if strings.HasPrefix(line, `data: {"count"`) {
					return line
				}
			case <-deadline:
				t.Fatal("no presence event on the stream")
				return ""
			}
		}
	}

	first, firstLines := open()
	defer first.Body.Close()
	if got, want := nextCount(firstLines), `data: {"count":1}`; got != want {
		t.Fatalf("on connect: got %q, want %q", got, want)
	}

	// A second device arriving is the event the first one is waiting for.
	second, secondLines := open()
	if got, want := nextCount(secondLines), `data: {"count":2}`; got != want {
		t.Errorf("second device saw %q, want %q", got, want)
	}
	if got, want := nextCount(firstLines), `data: {"count":2}`; got != want {
		t.Errorf("first device was not told about the arrival: got %q, want %q", got, want)
	}

	// And leaving has to be reported too, or a stale count outlives the device.
	second.Body.Close()
	if got, want := nextCount(firstLines), `data: {"count":1}`; got != want {
		t.Errorf("departure not reported: got %q, want %q", got, want)
	}
}

// Presence must not carry an id:, for the reason paired must not: advancing
// Last-Event-ID onto a non-item makes the next reconnect skip real items.
func TestPresenceEventCarriesNoEventID(t *testing.T) {
	var sb strings.Builder
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder(), body: &sb}
	if err := writePresence(rec, 3); err != nil {
		t.Fatal(err)
	}
	got := sb.String()
	if strings.Contains(got, "id:") {
		t.Fatalf("presence carries an id, which would poison Last-Event-ID:\n%s", got)
	}
	if !strings.HasPrefix(got, "event: presence\n") {
		t.Fatalf("unexpected framing:\n%s", got)
	}
	if !strings.Contains(got, `{"count":3}`) {
		t.Fatalf("count did not survive:\n%s", got)
	}
}
