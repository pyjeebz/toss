package web

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pyjeebz/toss/internal/api"
	"github.com/pyjeebz/toss/internal/hub"
)

// maxPlaintextBytes is what "max item size" means to someone pasting.
//
// The documented 256 KB is the request body, and since M3 that body carries
// base64 of the ciphertext rather than the text. What fits is smaller by the
// base64 expansion, the 16-byte GCM tag, the IV and the JSON envelope:
//
//	{"iv":"<16 chars>","content":"<ceil(4*(N+16)/3) chars>"}
//
// which is 38 characters of frame around the payload. Solving 38 +
// ceil(4*(N+16)/3) = 262144 gives 196563 bytes: about 192 KB, and fewer
// characters than that for anything non-ASCII, because this is bytes of UTF-8.
//
// The number is checked against a real server below rather than trusted.
const maxPlaintextBytes = 196563

// bodyFor seals text the way app.js does and returns the exact bytes it would
// POST.
func bodyFor(t *testing.T, text string) []byte {
	t.Helper()
	got := runCrypto(t, []cryptoOp{{Op: "roundtrip", Text: text}})
	if got[0].Text != text {
		t.Fatal("the round trip did not preserve the text")
	}
	body, err := json.Marshal(map[string]string{"iv": got[0].IV, "content": got[0].Content})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// The limit in the docs is the wire limit. This is the one a person hits, and
// the gap between them is a third of the number -- big enough that "it just
// didn't send" is the symptom of believing the documented one.
func TestTheRealPlaintextLimitIsWhatTheDocsSay(t *testing.T) {
	h := hub.New()
	srv := api.New(h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Routes(FS()))
	defer ts.Close()

	room, err := h.Create()
	if err != nil {
		t.Fatal(err)
	}

	send := func(text string) int {
		t.Helper()
		body := bodyFor(t, text)
		res, err := http.Post(ts.URL+"/api/rooms/"+room.ID+"/items",
			"application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		io.Copy(io.Discard, res.Body)
		return res.StatusCode
	}

	// Exactly at the limit: the body is 256 KB to the byte, and it goes.
	at := strings.Repeat("a", maxPlaintextBytes)
	if n := len(bodyFor(t, at)); n != 256<<10 {
		t.Errorf("body for the limit is %d bytes, want exactly %d", n, 256<<10)
	}
	if code := send(at); code != http.StatusCreated {
		t.Errorf("a %d-byte plaintext gave %d, want 201 -- the documented limit is too high",
			maxPlaintextBytes, code)
	}

	// One byte more, and base64 rounds the body up past the cap.
	if code := send(at + "a"); code != http.StatusRequestEntityTooLarge {
		t.Errorf("a %d-byte plaintext gave %d, want 413 -- the documented limit is too low",
			maxPlaintextBytes+1, code)
	}
}

// The limit is bytes of UTF-8, not characters, so the same cap in a language
// that is not ASCII is a quarter of the characters. Worth pinning because the
// docs say a size and people read it as a length.
func TestTheLimitCountsBytesNotCharacters(t *testing.T) {
	// Four bytes each.
	const emoji = "🔑"
	text := strings.Repeat(emoji, maxPlaintextBytes/4)

	if n := len(bodyFor(t, text)); n > 256<<10 {
		t.Errorf("body is %d bytes, over the %d cap: the byte budget was miscounted", n, 256<<10)
	}
	if chars := len([]rune(text)); chars > maxPlaintextBytes/3 {
		t.Errorf("%d characters fit, which is more than a 4-byte encoding allows", chars)
	}
}
