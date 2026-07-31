package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/pyjeebz/toss/internal/hub"
)

// pingInterval keeps proxies from reaping an idle stream.
const pingInterval = 20 * time.Second

// stream is the SSE endpoint. EventSource on the client side gives us
// reconnect and Last-Event-ID replay for free, which is the whole reason this
// is not a WebSocket.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	room := s.room(w, r)
	if room == nil {
		return
	}

	// Assert the flusher once, up front. Without an explicit Flush after every
	// event the response buffers and the client sees nothing until close.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // nginx: do not buffer this
	w.WriteHeader(http.StatusOK)

	// Tell EventSource how long to wait before reconnecting. The default is
	// browser-specific and can be several seconds; a phone waking up should be
	// back on the stream faster than that.
	fmt.Fprint(w, "retry: 2000\n\n")
	flusher.Flush()

	// Subscribe before replaying, so nothing published mid-replay is lost. The
	// overlap can send an item twice; the client dedupes by ID.
	sub := room.Subscribe()
	defer sub.Close()

	// EventSource cannot set headers on the first connect, so accept the
	// resume point from the query string too.
	last := r.Header.Get("Last-Event-ID")
	if last == "" {
		last = r.URL.Query().Get("last_event_id")
	}
	for _, it := range room.Since(last) {
		if err := writeItem(w, it); err != nil {
			return
		}
		flusher.Flush()
	}

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()

	ctx := r.Context()
	s.Log.Info("stream open", "room", room.ID, "subscribers", room.Subscribers())
	defer func() { s.Log.Info("stream closed", "room", room.ID) }()

	for {
		select {
		case <-ctx.Done():
			// Client hung up, or the server is shutting down. Without this the
			// goroutine and its subscriber entry leak, invisibly, until they
			// are not invisible any more.
			return

		case ev := <-sub.C():
			var err error
			switch ev.Kind {
			case hub.EventItem:
				err = writeItem(w, ev.Item)
			case hub.EventDeleted:
				err = writeDeleted(w, ev.ID)
			}
			if err != nil {
				return
			}
			flusher.Flush()

		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeItem emits an `item` event keyed by the item's ULID, so a reconnecting
// client's Last-Event-ID resumes exactly where it left off.
func writeItem(w http.ResponseWriter, it hub.Item) error {
	// JSON escapes newlines, which is what keeps a multi-line paste from
	// breaking the one-line-per-data-field wire format.
	payload, err := json.Marshal(it)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %s\nevent: item\ndata: %s\n\n", it.ID, payload)
	return err
}

// writeDeleted deliberately omits an `id:` field. Advancing Last-Event-ID to a
// deleted item's ULID would make a reconnect skip everything published between
// that item and now.
func writeDeleted(w http.ResponseWriter, id string) error {
	payload, err := json.Marshal(map[string]string{"id": id})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: deleted\ndata: %s\n\n", payload)
	return err
}
