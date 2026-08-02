package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyjeebz/toss/internal/api"
	"github.com/pyjeebz/toss/internal/hub"
)

// The whole M3 claim, end to end, against a real server: two devices pair by
// typed code, and the one that never saw the key ends up reading the text --
// while the server holds nothing it can open.
//
// The unit tests either side of this check that the pieces work. This checks
// that they are wired together, which is the part that a refactor breaks.
//
// Node plays both devices, using the same crypto.js the browser loads.

const e2eDriver = `
const fs = require('fs');
globalThis.window = {};
new Function(fs.readFileSync(process.argv[2], 'utf8'))();
const C = globalThis.window.TossCrypto;

const base = process.argv[3];
const PLAINTEXT = process.argv[4];

const json = async (res) => {
  const body = await res.text();
  if (!res.ok) throw new Error(res.status + ' ' + body);
  return body ? JSON.parse(body) : null;
};

(async () => {
  // --- device A: mints a room and a key, sends one item ---

  const { id: room } = await json(await fetch(base + '/api/rooms', { method: 'POST' }));
  const keyA = await C.generateKey();
  const keyAB64 = await C.exportKey(keyA);

  const sealed = await C.encrypt(keyA, PLAINTEXT);
  await json(await fetch(base + '/api/rooms/' + room + '/items', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ iv: sealed.iv, content: sealed.content }),
  }));

  // What the server is actually holding, verbatim.
  const stored = await (await fetch(base + '/api/rooms/' + room + '/items')).text();

  // --- device A: offers a pairing code ---

  const code = C.newPairCode();
  const payload = await C.wrapForCode(code, keyA);
  await json(await fetch(base + '/api/pair', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ room, code, payload }),
  }));

  // --- device B: knows only what was read off A's screen, typed back ---

  const typed = C.formatCode(code).toLowerCase();
  const redeemed = await json(await fetch(base + '/api/pair/' + encodeURIComponent(typed), {
    method: 'POST',
  }));

  const keyB = await C.unwrapWithCode(typed, redeemed.payload);
  const { items } = await json(await fetch(base + '/api/rooms/' + redeemed.room + '/items'));
  const readBack = await C.decrypt(keyB, items[0].iv, items[0].content);

  fs.writeFileSync(process.argv[5], JSON.stringify({
    room,
    keyB64: keyAB64,
    typed,
    joinedRoom: redeemed.room,
    stored,
    storedContent: items[0].content,
    storedIV: items[0].iv,
    payload: redeemed.payload,
    readBack,
    keyBB64: await C.exportKey(keyB),
  }));
})().catch((err) => {
  console.error(err);
  process.exit(1);
});
`

type e2eOut struct {
	Room          string `json:"room"`
	KeyB64        string `json:"keyB64"`
	Typed         string `json:"typed"`
	JoinedRoom    string `json:"joinedRoom"`
	Stored        string `json:"stored"`
	StoredContent string `json:"storedContent"`
	StoredIV      string `json:"storedIV"`
	Payload       string `json:"payload"`
	ReadBack      string `json:"readBack"`
	KeyBB64       string `json:"keyBB64"`
}

func TestPairingCarriesTheKeyEndToEnd(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping the end-to-end pairing check")
	}

	const plaintext = "correct horse battery staple — and a 🔑"

	h := hub.New()
	srv := api.New(h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Routes(FS()))
	defer ts.Close()

	dir := t.TempDir()
	script := filepath.Join(dir, "driver.js")
	out := filepath.Join(dir, "out.json")
	if err := os.WriteFile(script, []byte(e2eDriver), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(node, script, "crypto.js", ts.URL, plaintext, out)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("end-to-end run failed: %v\n%s", err, output)
	}

	blob, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got e2eOut
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatal(err)
	}

	// The point of the exercise: a device that only ever saw eight typed
	// characters can read what the other one pasted.
	if got.ReadBack != plaintext {
		t.Errorf("device B read back\n got %q\nwant %q", got.ReadBack, plaintext)
	}
	if got.KeyBB64 != got.KeyB64 {
		t.Errorf("device B ended up with a different key\n got %q\nwant %q", got.KeyBB64, got.KeyB64)
	}
	if got.JoinedRoom != got.Room {
		t.Errorf("device B joined %q, want %q", got.JoinedRoom, got.Room)
	}

	// And the other half: the server held none of it. This checks the actual
	// response body the server produced, not a reconstruction of it.
	if strings.Contains(got.Stored, plaintext) {
		t.Error("the plaintext is in what the server serves back")
	}
	if strings.Contains(got.Stored, got.KeyB64) {
		t.Error("the room key is in what the server serves back")
	}
	if strings.Contains(got.Payload, got.KeyB64) {
		t.Error("the room key is sitting in the pairing payload verbatim")
	}
	if got.StoredIV == "" || got.StoredContent == "" {
		t.Error("the server did not round-trip the iv and content")
	}

	// The typed form really was the sloppy one -- otherwise this test would
	// pass without exercising normalisation at all.
	if !strings.Contains(got.Typed, "-") || got.Typed != strings.ToLower(got.Typed) {
		t.Errorf("device B typed %q, which is not the hyphenated lower-case form", got.Typed)
	}
}
