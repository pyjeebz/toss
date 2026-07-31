// Package api is the HTTP surface: room lifecycle, item send/backfill/delete,
// and the SSE stream. It never inspects item content.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/pyjeebz/toss/internal/hub"
)

// maxItemBytes caps a single send. Bodies land in memory, so this is enforced
// at the reader rather than after the fact.
const maxItemBytes = 256 << 10 // 256 KB

// qrPixels is the rendered QR edge length. The design shows it at 192 CSS px;
// this is generous enough to stay sharp on a phone.
const qrPixels = 512

var errNoCode = errors.New("could not find a free pairing code")

// Server wires handlers to the hub.
type Server struct {
	Hub *hub.Hub
	Log *slog.Logger

	pairs *pairStore
	index []byte // web/index.html, read once at Routes()

	writes  *limiter // sends, deletes and pairing, per IP
	creates *limiter // room creation, per IP
}

// New returns a Server.
func New(h *hub.Hub, log *slog.Logger) *Server {
	return &Server{
		Hub:     h,
		Log:     log,
		pairs:   newPairStore(),
		writes:  newLimiter("writes", 60, time.Minute),
		creates: newLimiter("rooms", 10, time.Hour),
	}
}

// StartSweeper drops rate-limit buckets and pairing codes that no longer mean
// anything. Runs until ctx is cancelled.
func (s *Server) StartSweeper(ctx context.Context) {
	go func() {
		t := time.NewTicker(hub.SweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				s.writes.sweep(now)
				s.creates.sweep(now)
				s.pairs.sweep()
			}
		}
	}()
}

// Routes builds the mux. Go 1.22+ ServeMux does methods and wildcards, so
// there is no router dependency.
func (s *Server) Routes(static fs.FS) http.Handler {
	// The room page is the index page: same document, different entry point.
	// Read it once rather than per request.
	index, err := fs.ReadFile(static, "index.html")
	if err != nil {
		panic("web/index.html missing from the embedded FS: " + err.Error())
	}
	s.index = index

	mux := http.NewServeMux()

	// Reads are unmetered: backfill on wake and stream reconnects are exactly
	// what a phone does all day, and throttling those breaks the product.
	// Everything that costs memory or guesses at a secret is metered.
	mux.HandleFunc("POST /api/rooms", s.limit(s.creates, s.createRoom))
	mux.HandleFunc("POST /api/rooms/{room}/items", s.limit(s.writes, s.sendItem))
	mux.HandleFunc("GET /api/rooms/{room}/items", s.listItems)
	mux.HandleFunc("GET /api/rooms/{room}/stream", s.stream)
	mux.HandleFunc("DELETE /api/rooms/{room}/items/{id}", s.limit(s.writes, s.deleteItem))
	mux.HandleFunc("DELETE /api/rooms/{room}/items", s.limit(s.writes, s.clearItems))
	mux.HandleFunc("GET /api/rooms/{room}/qr.png", s.roomQR)
	mux.HandleFunc("POST /api/pair", s.limit(s.writes, s.mintPair))
	// Redemption is the one endpoint where a caller is guessing at a secret.
	mux.HandleFunc("POST /api/pair/{code}", s.limit(s.writes, s.redeemPair))
	mux.HandleFunc("GET /healthz", s.healthz)

	// A more specific pattern wins, so this takes precedence over the static
	// handler below.
	mux.HandleFunc("GET /r/{room}", s.roomPage)
	mux.Handle("GET /", http.FileServerFS(static))

	return securityHeaders(mux)
}

// --- handlers ---

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	room, err := s.Hub.Create()
	if err != nil {
		s.Log.Error("mint room id", "err", err)
		writeErr(w, http.StatusInternalServerError, "could not create room")
		return
	}
	s.Log.Info("room created", "room", room.ID)
	writeJSON(w, http.StatusCreated, map[string]string{"id": room.ID})
}

type sendRequest struct {
	IV      string `json:"iv"`
	Content string `json:"content"`
}

func (s *Server) sendItem(w http.ResponseWriter, r *http.Request) {
	room := s.room(w, r)
	if room == nil {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxItemBytes)
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "item too large")
			return
		}
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	if req.Content == "" {
		writeErr(w, http.StatusBadRequest, "empty content")
		return
	}

	now := time.Now().UTC()
	it := hub.Item{
		ID:        ulid.Make().String(),
		IV:        req.IV,
		Content:   req.Content, // opaque -- do not parse, do not log
		Origin:    originFromUA(r.UserAgent()),
		CreatedAt: now,
		ExpiresAt: now.Add(hub.ItemTTL),
	}
	room.Publish(it)

	// Metadata only. Never the content.
	s.Log.Info("item sent", "room", room.ID, "item", it.ID, "bytes", len(it.Content))
	writeJSON(w, http.StatusCreated, it)
}

func (s *Server) listItems(w http.ResponseWriter, r *http.Request) {
	room := s.room(w, r)
	if room == nil {
		return
	}
	room.Touch()
	items := room.Since(r.URL.Query().Get("since"))
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) deleteItem(w http.ResponseWriter, r *http.Request) {
	room := s.room(w, r)
	if room == nil {
		return
	}
	id := r.PathValue("id")
	if !room.Delete(id) {
		writeErr(w, http.StatusNotFound, "no such item")
		return
	}
	s.Log.Info("item deleted", "room", room.ID, "item", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) clearItems(w http.ResponseWriter, r *http.Request) {
	room := s.room(w, r)
	if room == nil {
		return
	}
	n := room.Clear()
	s.Log.Info("room cleared", "room", room.ID, "items", n)
	w.WriteHeader(http.StatusNoContent)
}

// roomPage serves the app itself for /r/{room}. It deliberately does NOT check
// that the room exists: at M3 the URL fragment carries the key, the browser
// never sends it, and the client needs to be running before it can tell whether
// the room is still there. A 404 for a stale room is the client's job.
func (s *Server) roomPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(s.index)
}

// roomQR renders the room URL as a PNG.
//
// NOTE FOR M3 -- read before adding the key to this.
//
// The QR encodes the room URL WITHOUT a fragment, and it has to stay that way.
// The server generates this image, so anything encoded into it is something the
// server knows. Putting the key in here would hand it to the server and undo
// the entire point of keeping it in the fragment.
//
// That means at M3 a scan alone cannot carry the key. Two ways out, and it is a
// product decision, not a technical one:
//
//  1. Client-side QR generation, and this endpoint goes away.
//  2. The scan gets you into the room, and the key rides the typed pairing code
//     via the opaque payload on /api/pair (see mintPairRequest).
func (s *Server) roomQR(w http.ResponseWriter, r *http.Request) {
	room := s.room(w, r)
	if room == nil {
		return
	}

	png, err := qrcode.Encode(originFor(r)+"/r/"+room.ID, qrcode.Medium, qrPixels)
	if err != nil {
		s.Log.Error("encode qr", "err", err)
		writeErr(w, http.StatusInternalServerError, "could not render code")
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Write(png)
}

// originFor works out the absolute origin to put in the QR. Set TOSS_ORIGIN
// behind a proxy that rewrites the host.
func originFor(r *http.Request) string {
	if o := os.Getenv("TOSS_ORIGIN"); o != "" {
		return strings.TrimSuffix(o, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	return scheme + "://" + r.Host
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"rooms":         s.Hub.Len(),
		"pair_codes":    s.pairs.len(),
		"write_buckets": s.writes.len(),
		"room_buckets":  s.creates.len(),
	})
}

// --- helpers ---

// room resolves {room} or writes the 404 and returns nil.
func (s *Server) room(w http.ResponseWriter, r *http.Request) *hub.Room {
	room := s.Hub.Get(r.PathValue("room"))
	if room == nil {
		writeErr(w, http.StatusNotFound, "no such room")
		return nil
	}
	return room
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// securityHeaders applies the baseline to every response. The CSP forbids
// inline script, which is why app.js is a separate file.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; "+
				"img-src 'self' data:; connect-src 'self'; "+
				"base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		h.Set("X-Robots-Tag", "noindex")
		next.ServeHTTP(w, r)
	})
}

// originFromUA renders a cosmetic "OS · Browser" label. It is a display string
// shown next to an item and nothing else -- never an identifier, never used to
// make a decision.
func originFromUA(ua string) string {
	if ua == "" {
		return "Unknown device"
	}
	os := "Unknown"
	switch {
	case strings.Contains(ua, "iPhone"):
		os = "iPhone"
	case strings.Contains(ua, "iPad"):
		os = "iPad"
	case strings.Contains(ua, "Android"):
		os = "Android"
	case strings.Contains(ua, "Windows"):
		os = "Windows"
	case strings.Contains(ua, "Macintosh"), strings.Contains(ua, "Mac OS X"):
		os = "Mac"
	case strings.Contains(ua, "CrOS"):
		os = "ChromeOS"
	case strings.Contains(ua, "Linux"):
		os = "Linux"
	}

	browser := "Browser"
	switch {
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "OPR/"), strings.Contains(ua, "Opera"):
		browser = "Opera"
	case strings.Contains(ua, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "Chrome/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	}
	return os + " · " + browser
}
