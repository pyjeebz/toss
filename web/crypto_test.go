package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// crypto.js is the whole of M3. If it is wrong, the failure modes are quiet:
// content that cannot be read back, a pairing code that finds the room but
// unlocks nothing, or -- worst and least visible -- encryption that looks fine
// and is not. None of that shows up in the UI as an error.
//
// So it is driven under node, which has the same WebCrypto the browser does,
// and the assertions live here in Go. The driver performs mechanism only; every
// pass/fail decision is made on this side, so a bug in the driver cannot vote
// itself correct.
//
// Node is required. Without it these skip rather than fail.

const cryptoDriver = `
const fs = require('fs');
globalThis.window = {};
new Function(fs.readFileSync(process.argv[2], 'utf8'))();
const C = globalThis.window.TossCrypto;

const b64 = C._internals.toB64;
const raw = C._internals.fromB64;

async function run(op) {
  switch (op.op) {
    case 'roundtrip': {
      const key = await C.generateKey();
      const sealed = await C.encrypt(key, op.text);
      const back = await C.decrypt(key, sealed.iv, sealed.content);
      return {
        text: back,
        iv: sealed.iv,
        content: sealed.content,
        ivBytes: raw(sealed.iv).length,
        ctBytes: raw(sealed.content).length,
      };
    }

    case 'exportImport': {
      const key = await C.generateKey();
      const out = await C.exportKey(key);
      const again = await C.exportKey(await C.importKey(out));
      return { first: out, second: again, bytes: raw(out).length };
    }

    case 'wrongKey': {
      const a = await C.generateKey();
      const b = await C.generateKey();
      const sealed = await C.encrypt(a, op.text);
      try {
        return { plaintext: await C.decrypt(b, sealed.iv, sealed.content) };
      } catch {
        return { rejected: true };
      }
    }

    case 'tamper': {
      const key = await C.generateKey();
      const sealed = await C.encrypt(key, op.text);
      // Flip one bit of ciphertext. GCM authenticates, so this must not decode.
      const bytes = raw(sealed.content);
      bytes[op.at % bytes.length] ^= 1;
      try {
        return { plaintext: await C.decrypt(key, sealed.iv, b64(bytes)) };
      } catch {
        return { rejected: true };
      }
    }

    case 'reuse': {
      // Same key, same plaintext, many times over.
      const key = await C.generateKey();
      const ivs = [];
      const cts = [];
      for (let i = 0; i < op.n; i++) {
        const sealed = await C.encrypt(key, op.text);
        ivs.push(sealed.iv);
        cts.push(sealed.content);
      }
      return { ivs, cts };
    }

    case 'wrap': {
      const key = await C.generateKey();
      const payload = await C.wrapForCode(op.code, key);
      try {
        const back = await C.unwrapWithCode(op.typed, payload);
        return {
          payload,
          original: await C.exportKey(key),
          unwrapped: await C.exportKey(back),
        };
      } catch {
        return { payload, original: await C.exportKey(key), rejected: true };
      }
    }

    case 'newCode': {
      const codes = [];
      for (let i = 0; i < op.n; i++) codes.push(C.newPairCode());
      return { codes };
    }

    case 'format':
      return { out: C.formatCode(op.text) };

    default:
      throw new Error('unknown op ' + op.op);
  }
}

(async () => {
  const ops = JSON.parse(fs.readFileSync(process.argv[3], 'utf8'));
  const out = [];
  for (const op of ops) out.push(await run(op));
  fs.writeFileSync(process.argv[4], JSON.stringify(out));
})().catch((err) => {
  console.error(err);
  process.exit(1);
});
`

type cryptoOp struct {
	Op    string `json:"op"`
	Text  string `json:"text,omitempty"`
	Code  string `json:"code,omitempty"`
	Typed string `json:"typed,omitempty"`

	// No omitempty on these two. At=0 is a meaningful offset, and omitting it
	// leaves op.at undefined in the driver, where `bytes[undefined % len] ^= 1`
	// silently tampers with nothing and the test passes for the wrong reason.
	At int `json:"at"`
	N  int `json:"n"`
}

type cryptoResult struct {
	Text      string   `json:"text"`
	IV        string   `json:"iv"`
	Content   string   `json:"content"`
	IVBytes   int      `json:"ivBytes"`
	CTBytes   int      `json:"ctBytes"`
	First     string   `json:"first"`
	Second    string   `json:"second"`
	Bytes     int      `json:"bytes"`
	Plaintext string   `json:"plaintext"`
	Rejected  bool     `json:"rejected"`
	IVs       []string `json:"ivs"`
	CTs       []string `json:"cts"`
	Payload   string   `json:"payload"`
	Original  string   `json:"original"`
	Unwrapped string   `json:"unwrapped"`
	Codes     []string `json:"codes"`
	Out       string   `json:"out"`
}

func runCrypto(t *testing.T, ops []cryptoOp) []cryptoResult {
	t.Helper()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping the crypto.js checks")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "driver.js")
	in := filepath.Join(dir, "in.json")
	out := filepath.Join(dir, "out.json")

	if err := os.WriteFile(script, []byte(cryptoDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(ops)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(node, script, "crypto.js", in, out)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("running crypto.js under node: %v\n%s", err, output)
	}

	blob, err = os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var results []cryptoResult
	if err := json.Unmarshal(blob, &results); err != nil {
		t.Fatal(err)
	}
	return results
}

// The content people actually paste: URLs, snippets, non-Latin scripts, emoji,
// and the awkward edges. Anything that does not survive the round trip is a
// paste the product silently loses.
var roundTripCases = []string{
	"hello",
	"a",
	" ",
	"\n",
	"\t\r\n  ",
	"https://example.com/a?b=c&d=e#f",
	"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI",
	strings.Repeat("long ", 4000),
	"héllo wörld — ünïcödé",
	"日本語のテキスト",
	"العربية",
	"🔐🎉👋🏽 family: 👨‍👩‍👧‍👦",
	"combining: é ä",
	"control bytes: \x00\x01\x1f\x7f",
	"quotes \" ' ` and <html> & entities",
	"{\"json\": [1, 2, 3], \"nested\": {\"a\": null}}",
}

func TestEncryptRoundTrip(t *testing.T) {
	ops := make([]cryptoOp, 0, len(roundTripCases))
	for _, text := range roundTripCases {
		ops = append(ops, cryptoOp{Op: "roundtrip", Text: text})
	}

	for i, got := range runCrypto(t, ops) {
		want := roundTripCases[i]
		if got.Text != want {
			t.Errorf("round trip %d changed the text\n got %q\nwant %q", i, got.Text, want)
		}
		// 96 bits, which is the size AES-GCM is specified for.
		if got.IVBytes != 12 {
			t.Errorf("case %d: IV is %d bytes, want 12", i, got.IVBytes)
		}
		// Ciphertext is the plaintext plus a 16-byte GCM tag. If it ever comes
		// back the same length as the input, the tag is missing and nothing is
		// authenticating anything.
		if wantLen := len([]byte(want)) + 16; got.CTBytes != wantLen {
			t.Errorf("case %d: ciphertext is %d bytes, want %d (plaintext + 16-byte tag)",
				i, got.CTBytes, wantLen)
		}
		// The obvious catastrophe: encryption that is not encryption. Checked
		// against the decoded bytes, not the base64 -- and only for inputs long
		// enough that a chance match is not a coin flip. Four bytes puts that at
		// about 2^-32 per position.
		if len(want) >= 4 {
			ct, err := base64.RawURLEncoding.DecodeString(got.Content)
			if err != nil {
				t.Errorf("case %d: ciphertext is not base64url: %v", i, err)
			} else if bytes.Contains(ct, []byte(want)) {
				t.Errorf("case %d: the plaintext is sitting in the ciphertext", i)
			}
		}
	}
}

func TestKeyExportImportIsStable(t *testing.T) {
	got := runCrypto(t, []cryptoOp{{Op: "exportImport"}})[0]
	if got.Bytes != 32 {
		t.Errorf("key is %d bytes, want 32 (AES-256)", got.Bytes)
	}
	if got.First != got.Second {
		t.Errorf("export/import/export changed the key: %q then %q", got.First, got.Second)
	}
	// It has to survive a URL fragment and a QR code untouched.
	if strings.ContainsAny(got.First, "+/=") {
		t.Errorf("key %q is not base64url; it will not survive a fragment", got.First)
	}
}

func TestWrongKeyIsRejected(t *testing.T) {
	got := runCrypto(t, []cryptoOp{{Op: "wrongKey", Text: "secret"}})[0]
	if !got.Rejected {
		t.Fatalf("a different key decrypted the item, returning %q", got.Plaintext)
	}
}

// GCM authenticates as well as encrypts. Without that a hostile server could
// flip bits in stored content and the client would render the result.
func TestTamperedCiphertextIsRejected(t *testing.T) {
	ops := []cryptoOp{
		{Op: "tamper", Text: "transfer 100 to alice", At: 0},
		{Op: "tamper", Text: "transfer 100 to alice", At: 7},
		{Op: "tamper", Text: "transfer 100 to alice", At: 20},
		// Inside the tag itself.
		{Op: "tamper", Text: "transfer 100 to alice", At: 30},
	}
	for i, got := range runCrypto(t, ops) {
		if !got.Rejected {
			t.Errorf("case %d: tampered ciphertext decoded to %q", i, got.Plaintext)
		}
	}
}

// Reusing an IV under one key is the single mistake that breaks GCM outright,
// so this checks the property rather than the implementation.
func TestIVsAndCiphertextsNeverRepeat(t *testing.T) {
	const n = 300
	got := runCrypto(t, []cryptoOp{{Op: "reuse", Text: "the same text every time", N: n}})[0]

	seen := make(map[string]int, n)
	for i, iv := range got.IVs {
		if prev, dup := seen[iv]; dup {
			t.Fatalf("IV reused between item %d and item %d: %q", prev, i, iv)
		}
		seen[iv] = i
	}

	// Identical plaintext must not produce identical ciphertext, or the wire
	// leaks which pastes were repeats.
	cts := make(map[string]int, n)
	for i, ct := range got.CTs {
		if prev, dup := cts[ct]; dup {
			t.Fatalf("ciphertext repeated between item %d and item %d", prev, i)
		}
		cts[ct] = i
	}
}

// The typed-pairing path. Device A wraps under the code it chose; device B
// derives the same secret from what the person actually typed.
func TestPairingWrapSurvivesHowPeopleType(t *testing.T) {
	const code = "K3N8XQ2M"
	typed := []string{
		"K3N8XQ2M",      // exactly
		"K3N8-XQ2M",     // the displayed form
		"k3n8xq2m",      // lower case
		" k3n8 - xq2m ", // in a hurry
		"K3N8XQ2M\n",    // with the newline a paste brings
	}

	ops := make([]cryptoOp, 0, len(typed))
	for _, in := range typed {
		ops = append(ops, cryptoOp{Op: "wrap", Code: code, Typed: in})
	}

	for i, got := range runCrypto(t, ops) {
		if got.Rejected {
			t.Errorf("typing %q did not unwrap the key", typed[i])
			continue
		}
		if got.Unwrapped != got.Original {
			t.Errorf("typing %q unwrapped a different key\n got %q\nwant %q",
				typed[i], got.Unwrapped, got.Original)
		}
		// The payload is what sits on the server. The key must not be in it.
		if strings.Contains(got.Payload, got.Original) {
			t.Errorf("case %d: the room key is sitting in the payload verbatim", i)
		}
		if n := len(strings.Split(got.Payload, ".")); n != 3 {
			t.Errorf("case %d: payload has %d parts, want salt.iv.ciphertext", i, n)
		}
	}
}

func TestPairingWrapRejectsTheWrongCode(t *testing.T) {
	ops := []cryptoOp{
		{Op: "wrap", Code: "K3N8XQ2M", Typed: "K3N8XQ2N"}, // one character out
		{Op: "wrap", Code: "K3N8XQ2M", Typed: "K3N8XQ2"},  // truncated
		{Op: "wrap", Code: "K3N8XQ2M", Typed: ""},
	}
	for i, got := range runCrypto(t, ops) {
		if !got.Rejected {
			t.Errorf("case %d: the wrong code unwrapped the key anyway", i)
		}
	}
}

// Two devices wrapping the same key under the same code must still produce
// different payloads, or the salt is not doing its job.
func TestPairingPayloadsAreSalted(t *testing.T) {
	ops := []cryptoOp{
		{Op: "wrap", Code: "K3N8XQ2M", Typed: "K3N8XQ2M"},
		{Op: "wrap", Code: "K3N8XQ2M", Typed: "K3N8XQ2M"},
	}
	got := runCrypto(t, ops)
	if got[0].Payload == got[1].Payload {
		t.Fatal("two wraps under the same code produced the same payload")
	}
}

func TestNewPairCodeShape(t *testing.T) {
	const n = 500
	got := runCrypto(t, []cryptoOp{{Op: "newCode", N: n}})[0]

	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	seen := make(map[string]bool, n)
	for _, code := range got.Codes {
		if len(code) != 8 {
			t.Fatalf("code %q is %d chars, want 8", code, len(code))
		}
		if strings.ContainsAny(code, "ILOU") {
			t.Fatalf("code %q contains a character that is easy to misread", code)
		}
		for _, r := range code {
			if !strings.ContainsRune(alphabet, r) {
				t.Fatalf("code %q contains %q, which is off-alphabet", code, r)
			}
		}
		if seen[code] {
			t.Fatalf("generated %q twice in %d draws", code, n)
		}
		seen[code] = true
	}
}

func TestFormatCode(t *testing.T) {
	ops := []cryptoOp{
		{Op: "format", Text: "K3N8XQ2M"},
		{Op: "format", Text: "SHORT"},
	}
	got := runCrypto(t, ops)
	if got[0].Out != "K3N8-XQ2M" {
		t.Errorf("got %q, want K3N8-XQ2M", got[0].Out)
	}
	if got[1].Out != "SHORT" {
		t.Errorf("an unexpected length should pass through, got %q", got[1].Out)
	}
}
