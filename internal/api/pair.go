package api

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	// errCodeTaken means the client picked a code already in use. It picks
	// another; at 40 bits this effectively never happens twice.
	errCodeTaken = errors.New("pairing code already in use")

	errBadCode = errors.New("pairing code is not well formed")
)

const (
	// pairTTL is deliberately short. An 8-character code is brute-forceable
	// given time, so it is given as little of it as is still usable.
	pairTTL = 5 * time.Minute

	pairCodeLen = 8

	// Crockford base32: no I, L, O or U, so there is nothing to misread on a
	// phone screen and nothing that reads as a rude word. 32 symbols is 5 bits
	// each, so an 8-character code carries 40 bits.
	pairAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// pairing is a short code standing in for a room ID, briefly.
//
// The code is NOT the room. Room IDs are 120 bits and must stay unguessable;
// pairing codes are 40 bits and typeable. Keeping them separate is what lets
// pairing be convenient without weakening the room itself.
type pairing struct {
	room      string
	payload   string // opaque -- see the note on mintPair
	expiresAt time.Time
}

type pairStore struct {
	mu    sync.Mutex
	codes map[string]pairing
}

func newPairStore() *pairStore {
	return &pairStore{codes: make(map[string]pairing)}
}

// mint issues a server-chosen code for a room. Collisions are retried rather
// than tolerated.
//
// Only for pairings with no payload. A server-chosen code cannot be used to
// wrap key material -- see claim.
func (p *pairStore) mint(room string) (string, time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()

	expires := time.Now().Add(pairTTL)
	for attempt := 0; attempt < 10; attempt++ {
		code, err := newPairCode()
		if err != nil {
			return "", time.Time{}, err
		}
		if _, taken := p.codes[code]; taken {
			continue
		}
		p.codes[code] = pairing{room: room, expiresAt: expires}
		return code, expires, nil
	}
	return "", time.Time{}, errNoCode
}

// claim registers a code the client chose, along with the payload wrapped under
// it. The client has to be the one choosing: the payload is the room key sealed
// with a secret derived from the code, so a server that picked the code would
// hold both halves and could open the payload it is storing. Handing selection
// to the browser is what keeps the typed-pairing path end-to-end encrypted.
//
// The code is still a 40-bit secret drawn from crypto.getRandomValues; what
// changes is only who draws it. A client that draws badly weakens its own room
// and nobody else's.
func (p *pairStore) claim(code, room, payload string) (time.Time, error) {
	if !validPairCode(code) {
		return time.Time{}, errBadCode
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()

	if _, taken := p.codes[code]; taken {
		return time.Time{}, errCodeTaken
	}
	expires := time.Now().Add(pairTTL)
	p.codes[code] = pairing{room: room, payload: payload, expiresAt: expires}
	return expires, nil
}

// validPairCode requires the canonical form: exact length, and every character
// already in the alphabet. Normalisation is for what a person types, not for
// what a client submits.
func validPairCode(code string) bool {
	if len(code) != pairCodeLen {
		return false
	}
	for _, r := range code {
		if !strings.ContainsRune(pairAlphabet, r) {
			return false
		}
	}
	return true
}

// redeem consumes a code. Single redemption: a code that has been used is gone,
// so a stolen-but-already-used code is worth nothing, and a brute-force attempt
// gets exactly one guess per code that ever existed.
func (p *pairStore) redeem(code string) (pairing, bool) {
	code = normalizePairCode(code)

	p.mu.Lock()
	defer p.mu.Unlock()

	pr, ok := p.codes[code]
	if !ok {
		return pairing{}, false
	}
	delete(p.codes, code)
	if time.Now().After(pr.expiresAt) {
		return pairing{}, false
	}
	return pr, true
}

// sweep drops expired codes on the background tick, so a burst of codes that
// are never redeemed does not sit in memory until the next mint.
func (p *pairStore) sweep() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
}

// sweepLocked drops expired codes. Also called on mint, which keeps the map
// tidy even if the background sweeper is not running (tests).
func (p *pairStore) sweepLocked() {
	now := time.Now()
	for code, pr := range p.codes {
		if now.After(pr.expiresAt) {
			delete(p.codes, code)
		}
	}
}

func (p *pairStore) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.codes)
}

func newPairCode() (string, error) {
	b := make([]byte, pairCodeLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, pairCodeLen)
	for i, v := range b {
		// 256 is not a multiple of 32, but 32 divides 256 exactly (8 groups),
		// so masking the low 5 bits is uniform.
		out[i] = pairAlphabet[v&31]
	}
	return string(out), nil
}

// normalizePairCode makes typing forgiving: case, the display hyphen and stray
// spaces are all ignored, and the characters Crockford excludes are folded onto
// what the person meant.
func normalizePairCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch r {
		case 'O':
			b.WriteRune('0')
		case 'I', 'L':
			b.WriteRune('1')
		case '-', ' ':
			// display separator, ignore
		default:
			if strings.ContainsRune(pairAlphabet, r) {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// formatPairCode groups the code for display: K3N8-XQ2M.
func formatPairCode(code string) string {
	if len(code) != pairCodeLen {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// --- handlers ---

type mintPairRequest struct {
	Room string `json:"room"`

	// Code, when set, is the client's own choice of pairing code. Required
	// whenever Payload is set, and the reason is the whole point of M3: the
	// payload is sealed under a secret derived from the code, so a
	// server-chosen code would let the server open the payload it stores.
	Code string `json:"code,omitempty"`

	// Payload is opaque and passed straight back on redemption. The server
	// neither reads it nor requires it.
	//
	// It carries the room key wrapped under a secret derived from Code: device
	// A wraps, the server stores ciphertext, device B derives the same secret
	// from what the person typed and unwraps locally. That keeps the typed-code
	// path working under E2EE without the server ever holding key material.
	Payload string `json:"payload,omitempty"`
}

func (s *Server) mintPair(w http.ResponseWriter, r *http.Request) {
	var req mintPairRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	if s.Hub.Get(req.Room) == nil {
		writeErr(w, http.StatusNotFound, "no such room")
		return
	}
	// Refusing this combination is what makes the footgun unrepresentable: a
	// payload sealed under a code the server chose is a payload the server can
	// open, and it would look identical from the outside.
	if req.Payload != "" && req.Code == "" {
		writeErr(w, http.StatusBadRequest, "a payload requires a client-supplied code")
		return
	}

	var (
		code    = req.Code
		expires time.Time
		err     error
	)
	if code != "" {
		expires, err = s.pairs.claim(code, req.Room, req.Payload)
	} else {
		code, expires, err = s.pairs.mint(req.Room)
	}
	switch {
	case errors.Is(err, errBadCode):
		writeErr(w, http.StatusBadRequest, "malformed code")
		return
	case errors.Is(err, errCodeTaken):
		// The client picks another and retries. At 40 bits this is vanishingly
		// rare, and it is cheaper than making the server guess what was meant.
		writeErr(w, http.StatusConflict, "that code is in use")
		return
	case err != nil:
		s.Log.Error("mint pairing code", "err", err)
		writeErr(w, http.StatusInternalServerError, "could not mint code")
		return
	}

	// Metadata only: the code is a short-lived secret and does not go in logs.
	s.Log.Info("pairing code minted", "room", req.Room)
	writeJSON(w, http.StatusCreated, map[string]any{
		"code":       formatPairCode(code),
		"expires_at": expires.UTC(),
		"expires_in": int(pairTTL.Seconds()),
	})
}

func (s *Server) redeemPair(w http.ResponseWriter, r *http.Request) {
	pr, ok := s.pairs.redeem(r.PathValue("code"))
	if !ok {
		// One message for "never existed", "already used" and "expired" alike:
		// telling them apart would tell an attacker which guesses were close.
		writeErr(w, http.StatusNotFound, "that code is not valid")
		return
	}
	if s.Hub.Get(pr.room) == nil {
		writeErr(w, http.StatusNotFound, "that room is gone")
		return
	}

	s.Log.Info("pairing code redeemed", "room", pr.room)
	writeJSON(w, http.StatusOK, map[string]any{
		"room":    pr.room,
		"payload": pr.payload,
	})
}
