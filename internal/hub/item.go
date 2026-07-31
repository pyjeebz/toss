package hub

import "time"

// Item is one piece of text living in a room.
//
// Content is OPAQUE to the server. From M3 onward it is AES-GCM ciphertext that
// only browsers holding the room key (which lives in the URL fragment and never
// reaches us) can read. Nothing on the Go side may parse, validate, transform,
// index or log it -- that constraint is what lets E2EE land without touching
// any of this package.
type Item struct {
	ID        string    `json:"id"`           // ULID -- sortable, doubles as the SSE event id
	IV        string    `json:"iv,omitempty"` // base64url, set from M3 onward
	Content   string    `json:"content"`      // opaque; ciphertext after M3
	Origin    string    `json:"origin"`       // "Windows · Chrome" -- cosmetic, never an identifier
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}
