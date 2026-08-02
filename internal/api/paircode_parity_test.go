package api

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The pairing code does two different jobs on two different machines, and they
// have to agree exactly.
//
// The server normalises what was typed to find the room. The browser normalises
// the same string to derive the secret that unwraps the room key. If the two
// disagree by one character, the failure is invisible from the server's side:
// the code redeems, the right room comes back, and then nothing decrypts. That
// is a bug that reaches a person before it reaches a log line.
//
// So normalizePairCode is pinned against normalizeCode in web/crypto.js, over
// every ASCII code point plus the ways people actually type. Node is required;
// without it this skips.
//
// Uppercasing on the JS side is deliberately ASCII-only, because Go's
// strings.ToUpper is Unicode-aware and the two disagree in both directions
// ('ß' -> 'SS' in JS, unchanged in Go; 'ſ' -> 'S' in Go, unchanged in JS).
// Neither is reachable from a keyboard typing an 8-character code, and the
// restriction is what makes this parity exact rather than approximate. Non-
// ASCII input is therefore out of scope here, by construction.

const normalizeDriver = `
const fs = require('fs');
globalThis.window = {};
new Function(fs.readFileSync(process.argv[2], 'utf8'))();
const C = globalThis.window.TossCrypto;

const inputs = JSON.parse(fs.readFileSync(process.argv[3], 'utf8'));
fs.writeFileSync(process.argv[4], JSON.stringify({
  normalized: inputs.map((s) => C.normalizeCode(s)),
  alphabet: C._internals.PAIR_ALPHABET,
  codeLen: C._internals.PAIR_CODE_LEN,
}));
`

type normalizeOut struct {
	Normalized []string `json:"normalized"`
	Alphabet   string   `json:"alphabet"`
	CodeLen    int      `json:"codeLen"`
}

func TestPairCodeNormalizationMatchesTheClient(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping the crypto.js parity check")
	}

	var inputs []string

	// Every ASCII code point on its own. This is the exhaustive part: the
	// alphabet folding, the ignored separators and the case rule all live in
	// here, and a divergence anywhere in the range shows up as one failure.
	for r := rune(0); r < 128; r++ {
		inputs = append(inputs, string(r))
	}

	// And the realistic ones: what a person types, and what a paste brings with
	// it.
	inputs = append(inputs,
		"",
		"K3N8XQ2M",
		"k3n8xq2m",
		"K3N8-XQ2M",
		" k3n8 - xq2m ",
		"K3N8-XQ2M\n",
		"\tK3N8XQ2M\r\n",
		"O1IL",
		"oIlL0",
		"UUUU",
		"k3n8xq2m!!!",
		"----",
		"        ",
		pairAlphabet,
		"abcdefghijklmnopqrstuvwxyz",
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		"0123456789",
		"!\"#$%&'()*+,./:;<=>?@[\\]^_`{|}~",
	)

	dir := t.TempDir()
	script := filepath.Join(dir, "driver.js")
	in := filepath.Join(dir, "in.json")
	out := filepath.Join(dir, "out.json")

	if err := os.WriteFile(script, []byte(normalizeDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	// crypto.js lives in the web package. Reaching across for it is deliberate:
	// the whole point is that these two files are one contract.
	cryptoJS := filepath.Join("..", "..", "web", "crypto.js")
	if _, err := os.Stat(cryptoJS); err != nil {
		t.Fatalf("cannot find crypto.js to check against: %v", err)
	}

	cmd := exec.Command(node, script, cryptoJS, in, out)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("running crypto.js under node: %v\n%s", err, output)
	}

	blob, err = os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got normalizeOut
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatal(err)
	}

	if got.Alphabet != pairAlphabet {
		t.Errorf("alphabets differ\n  go: %q\n  js: %q", pairAlphabet, got.Alphabet)
	}
	if got.CodeLen != pairCodeLen {
		t.Errorf("code length differs: go %d, js %d", pairCodeLen, got.CodeLen)
	}

	if len(got.Normalized) != len(inputs) {
		t.Fatalf("got %d results for %d inputs", len(got.Normalized), len(inputs))
	}
	for i, in := range inputs {
		want := normalizePairCode(in)
		if got.Normalized[i] != want {
			t.Errorf("normalize(%s)\n  go: %q\n  js: %q", describe(in), want, got.Normalized[i])
		}
	}
}

// describe keeps control characters legible in a failure message.
func describe(s string) string {
	if len(s) == 1 && (s[0] < 0x20 || s[0] == 0x7f) {
		return fmt.Sprintf("%q (0x%02x)", s, s[0])
	}
	return fmt.Sprintf("%q", s)
}
