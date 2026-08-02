package web

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	qrcode "github.com/skip2/go-qrcode"
)

// qr.js is a hand-written QR encoder, and a wrong table entry there produces a
// code that still looks like a QR and simply does not scan -- the kind of bug
// that reaches a phone camera before it reaches a test. So it is checked
// against a second, independent implementation.
//
// github.com/skip2/go-qrcode is a TEST-ONLY dependency for exactly this. It
// used to render the QR server-side; the binary no longer links it, because a
// server-rendered code cannot contain the URL fragment the room key lives in.
// It stays in go.mod as the reference implementation and nothing else.
//
// Node is required. Without it the test skips rather than fails: it guards
// qr.js, and qr.js only changes when someone is editing it.

// driver evaluates qr.js the way a browser would -- it assigns to `window` --
// and encodes each {text, mask} pair with the mask pinned.
const driver = `
const fs = require('fs');
globalThis.window = {};
new Function(fs.readFileSync(process.argv[2], 'utf8'))();
const { encode, toDataURL, _internals } = globalThis.window.TossQR;

const out = JSON.parse(fs.readFileSync(process.argv[3], 'utf8')).map(({ text, mask, render }) => {
  const pinned = encode(text, { forceMask: mask });
  const natural = encode(text);
  // The function-pattern map: everything masking must leave alone.
  const { reserved } = _internals.buildMatrix(natural.version);
  return {
    version: pinned.version,
    size: pinned.size,
    rows: pinned.modules.map((r) => r.join('')),
    naturalMask: natural.mask,
    naturalRows: natural.modules.map((r) => r.join('')),
    reserved: reserved.map((r) => r.map((v) => (v ? 1 : 0)).join('')),
    // Only where it is asked for: rendering every case builds several hundred
    // kilobytes of SVG per large version, for nothing.
    dataURL: render ? toDataURL(text) : '',
  };
});
fs.writeFileSync(process.argv[4], JSON.stringify(out));
`

type qrCase struct {
	Text   string `json:"text"`
	Mask   int    `json:"mask"`
	Render bool   `json:"render"`
}

type qrResult struct {
	Version     int      `json:"version"`
	Size        int      `json:"size"`
	Rows        []string `json:"rows"`
	NaturalMask int      `json:"naturalMask"`
	NaturalRows []string `json:"naturalRows"`
	Reserved    []string `json:"reserved"`
	DataURL     string   `json:"dataURL"`
}

// masks are the eight mask patterns from the spec, written out again here
// rather than read from qr.js: a test that borrows the implementation it is
// checking proves nothing.
var masks = []func(r, c int) bool{
	func(r, c int) bool { return (r+c)%2 == 0 },
	func(r, c int) bool { return r%2 == 0 },
	func(r, c int) bool { return c%3 == 0 },
	func(r, c int) bool { return (r+c)%3 == 0 },
	func(r, c int) bool { return (r/2+c/3)%2 == 0 },
	func(r, c int) bool { return (r*c)%2+(r*c)%3 == 0 },
	func(r, c int) bool { return ((r*c)%2+(r*c)%3)%2 == 0 },
	func(r, c int) bool { return ((r+c)%2+(r*c)%3)%2 == 0 },
}

func TestQRMatchesReferenceEncoder(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping the qr.js conformance check")
	}

	inputs := qrInputs()

	// go-qrcode picks its own mask, and the two encoders disagree on roughly a
	// fifth of inputs. That is a quality heuristic, not correctness -- every
	// mask decodes -- and the disagreement is go-qrcode's: its rule 1 scores a
	// run of exactly five modules as zero where the spec scores it 3. So read
	// back the mask it chose and pin qr.js to the same one, which compares
	// everything that actually has to agree.
	//
	// Reading the mask out of the reference bitmap is itself a check on our
	// format-information placement: get those coordinates wrong and we decode
	// the wrong mask here, and the matrices will not line up.
	cases := make([]qrCase, 0, len(inputs))
	refs := make([]*qrcode.QRCode, 0, len(inputs))
	for _, text := range inputs {
		q, err := qrcode.New(text, qrcode.Medium)
		if err != nil {
			t.Fatalf("reference encode (%d bytes): %v", len(text), err)
		}
		q.DisableBorder = true
		mask, ecLevel := formatInfo(t, q.Bitmap())
		if ecLevel != 0 {
			t.Fatalf("reference used EC level bits %02b, want 00 (M)", ecLevel)
		}
		cases = append(cases, qrCase{Text: text, Mask: mask})
		refs = append(refs, q)
	}

	got := runDriver(t, node, cases)
	if len(got) != len(cases) {
		t.Fatalf("driver returned %d results, want %d", len(got), len(cases))
	}

	versions := map[int]bool{}
	for i, res := range got {
		want := refs[i].Bitmap()
		label := fmt.Sprintf("len=%d version=%d mask=%d", len(cases[i].Text), res.Version, cases[i].Mask)

		if res.Version != refs[i].VersionNumber {
			t.Errorf("%s: version %d, reference chose %d", label, res.Version, refs[i].VersionNumber)
			continue
		}
		if res.Size != len(want) {
			t.Errorf("%s: size %d, reference %d", label, res.Size, len(want))
			continue
		}
		versions[res.Version] = true

		if r, c, ok := firstDiff(res.Rows, want); !ok {
			t.Errorf("%s: module (%d,%d) differs from the reference", label, r, c)
		}
	}

	// Every version the encoder claims to support has to have been exercised;
	// a silently truncated input list would make this whole test vacuous.
	for v := 1; v <= 20; v++ {
		if !versions[v] {
			t.Errorf("version %d was never exercised", v)
		}
	}

	// And the boundaries have to be the real ones. If maxPayload drifts, the
	// inputs stop straddling the version changes and the coverage above goes
	// quietly hollow -- so hold the table to what the reference actually does.
	versionAt := map[int]int{}
	for i, length := range qrLengths() {
		versionAt[length] = refs[i].VersionNumber
	}
	for v := 1; v <= 20; v++ {
		if got := versionAt[maxPayload[v]]; got != v {
			t.Errorf("maxPayload[%d] = %d bytes, but the reference encodes that as version %d",
				v, maxPayload[v], got)
		}
		if v == 20 {
			continue
		}
		if got := versionAt[maxPayload[v]+1]; got != v+1 {
			t.Errorf("one byte past maxPayload[%d] encodes as version %d, want %d",
				v, got, v+1)
		}
	}
}

// TestQRNaturalMaskCarriesTheSameSymbol covers the one thing pinning the mask
// cannot: the output the app actually ships, where qr.js chooses the mask
// itself and can legitimately choose a different one from the reference.
//
// A different mask is only safe if two things hold. The format information has
// to declare the mask that was really applied -- a scanner unmasks by what it
// reads there, so a truthful declaration is the whole contract. And undoing
// that mask has to leave exactly the data the reference encoded. Check both,
// and a divergent mask choice is proven to be a re-masking of the same symbol
// rather than a different one.
func TestQRNaturalMaskCarriesTheSameSymbol(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping the qr.js conformance check")
	}

	inputs := qrInputs()
	cases := make([]qrCase, 0, len(inputs))
	refs := make([]*qrcode.QRCode, 0, len(inputs))
	refMasks := make([]int, 0, len(inputs))
	for _, text := range inputs {
		q, err := qrcode.New(text, qrcode.Medium)
		if err != nil {
			t.Fatalf("reference encode (%d bytes): %v", len(text), err)
		}
		q.DisableBorder = true
		mask, _ := formatInfo(t, q.Bitmap())
		cases = append(cases, qrCase{Text: text, Mask: mask})
		refs = append(refs, q)
		refMasks = append(refMasks, mask)
	}

	got := runDriver(t, node, cases)
	differing := 0
	for i, res := range got {
		label := fmt.Sprintf("len=%d version=%d", len(cases[i].Text), res.Version)

		declared, ecLevel := formatInfoRows(res.NaturalRows)
		if declared != res.NaturalMask {
			t.Errorf("%s: format information declares mask %d, but mask %d was applied",
				label, declared, res.NaturalMask)
			continue
		}
		if ecLevel != 0 {
			t.Errorf("%s: format information declares EC level bits %02b, want 00 (M)", label, ecLevel)
			continue
		}
		if res.NaturalMask != refMasks[i] {
			differing++
		}

		// Strip both masks and compare what is underneath. Function patterns
		// are skipped: they are never masked, and the format areas legitimately
		// differ because they encode different mask numbers.
		ref := refs[i].Bitmap()
		mine, theirs := masks[res.NaturalMask], masks[refMasks[i]]
	data:
		for r := 0; r < res.Size; r++ {
			for c := 0; c < res.Size; c++ {
				if res.Reserved[r][c] == '1' {
					continue
				}
				got := (res.NaturalRows[r][c] == '1') != mine(r, c)
				want := ref[r][c] != theirs(r, c)
				if got != want {
					t.Errorf("%s: unmasked data module (%d,%d) differs from the reference", label, r, c)
					break data // one report per input is enough
				}
			}
		}
	}

	// If the two encoders ever agree on every mask, this test has quietly
	// stopped testing anything and the divergence it exists for is untested.
	if differing == 0 {
		t.Error("no input produced a differing mask choice; this test is no longer exercising divergence")
	}
	t.Logf("%d of %d inputs chose a different mask from the reference", differing, len(got))
}

// TestQRDataURL checks the render path, which the matrix comparison never
// touches: one <rect> per dark module, inside a quiet zone of four.
func TestQRDataURL(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping the qr.js conformance check")
	}

	const text = "https://toss.tools/r/01hqz9kx3m4n5p6q7r8s9t0v#k=RGVhdGggdG8gdGhlIGV2aWwgYnl0ZXMh"
	q, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		t.Fatal(err)
	}
	q.DisableBorder = true
	mask, _ := formatInfo(t, q.Bitmap())

	got := runDriver(t, node, []qrCase{{Text: text, Mask: mask, Render: true}})[0]

	if !strings.HasPrefix(got.DataURL, "data:image/svg+xml;charset=utf-8,") {
		t.Fatalf("data URL has the wrong prefix: %.40q", got.DataURL)
	}
	svg, err := decodeDataURL(got.DataURL)
	if err != nil {
		t.Fatalf("data URL is not decodable: %v", err)
	}

	// moduleSize 8, quiet zone 4 each side.
	dim := (got.Size + 8) * 8
	if want := fmt.Sprintf(`viewBox="0 0 %d %d"`, dim, dim); !strings.Contains(svg, want) {
		t.Errorf("svg is missing %s", want)
	}

	// One background rect plus one per dark module. Counted off qr.js's own
	// output rather than the reference, since toDataURL uses the natural mask.
	dark := 0
	for _, row := range got.Rows {
		dark += strings.Count(row, "1")
	}
	if n := strings.Count(svg, "<rect"); n != dark+1 {
		t.Errorf("svg has %d rects, want %d (%d dark modules plus the background)", n, dark+1, dark)
	}
}

// maxPayload is the largest byte-mode payload each version holds at EC level M.
//
// It is not trusted: the test asserts the reference picks version v for this
// many bytes and version v+1 for one more. Getting an entry wrong therefore
// fails the test rather than quietly testing the wrong version.
var maxPayload = [21]int{
	1: 14, 2: 26, 3: 42, 4: 62, 5: 84, 6: 106, 7: 122, 8: 152, 9: 180, 10: 213,
	11: 251, 12: 287, 13: 331, 14: 362, 15: 412, 16: 450, 17: 504, 18: 560,
	19: 624, 20: 666,
}

// qrInputs returns the payload lengths worth checking: everything short, and
// both sides of every version boundary. The boundaries are where an encoder
// gets it wrong -- that is where the terminator is truncated, where padding
// runs out, and where a version gains its second block group.
//
// Checking all 666 lengths finds nothing more and costs a minute under -race.
//
// Byte mode only, deliberately: go-qrcode splits its input into numeric and
// alphanumeric segments where that is shorter, and qr.js does not. Restricting
// the alphabet to characters outside the alphanumeric charset (0-9 A-Z and
// $%*+-./: plus space) leaves the reference no segmentation to find, so both
// encode a single byte segment and the comparison is like for like.
// qrLengths is the ordered set of payload lengths qrInputs generates, ahead of
// the fixed room URLs it appends.
func qrLengths() []int {
	wanted := map[int]bool{}
	for length := 1; length <= 30; length++ {
		wanted[length] = true
	}
	for v := 1; v <= 20; v++ {
		low := maxPayload[v-1] + 1 // maxPayload[0] is 0, so version 1 starts at 1
		wanted[low] = true
		wanted[(low+maxPayload[v])/2] = true
		wanted[maxPayload[v]] = true
	}

	ordered := make([]int, 0, len(wanted))
	for length := 1; length <= maxPayload[20]; length++ {
		if wanted[length] {
			ordered = append(ordered, length)
		}
	}
	return ordered
}

func qrInputs() []string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz_#=~?&"

	seed := uint32(1)
	next := func() byte {
		seed = seed*1664525 + 1013904223
		return alphabet[(seed>>16)%uint32(len(alphabet))]
	}

	lengths := qrLengths()
	inputs := make([]string, 0, len(lengths)+3)
	for _, length := range lengths {
		b := make([]byte, length)
		for i := range b {
			b[i] = next()
		}
		inputs = append(inputs, string(b))
	}

	// And the shapes this actually ships with: a room URL, with and without a
	// fragment carrying a 256-bit key. These do contain digits and '/', so the
	// reference could in principle segment them -- it does not, at these
	// lengths, and if that ever changes this test says so loudly rather than
	// quietly stopping being a comparison.
	return append(inputs,
		"http://localhost:8080/r/01hqz9kx3m4n5p6q7r8s9t0v",
		"https://toss.tools/r/01hqz9kx3m4n5p6q7r8s9t0v",
		"https://toss.tools/r/01hqz9kx3m4n5p6q7r8s9t0v#k=RGVhdGggdG8gdGhlIGV2aWwgYnl0ZXMh",
	)
}

func runDriver(t *testing.T, node string, cases []qrCase) []qrResult {
	t.Helper()

	dir := t.TempDir()
	script := filepath.Join(dir, "driver.js")
	in := filepath.Join(dir, "in.json")
	out := filepath.Join(dir, "out.json")

	if err := os.WriteFile(script, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(node, script, "qr.js", in, out)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("running qr.js under node: %v\n%s", err, output)
	}

	blob, err = os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var results []qrResult
	if err := json.Unmarshal(blob, &results); err != nil {
		t.Fatal(err)
	}
	return results
}

// formatInfo reads the 15 format bits from the copy beside the top-left finder
// and returns the mask and EC level they encode.
func formatInfo(t *testing.T, m [][]bool) (mask, ecLevel int) {
	t.Helper()
	return formatInfoAt(func(r, c int) bool { return m[r][c] })
}

func formatInfoRows(rows []string) (mask, ecLevel int) {
	return formatInfoAt(func(r, c int) bool { return rows[r][c] == '1' })
}

func formatInfoAt(dark func(r, c int) bool) (mask, ecLevel int) {
	bit := func(r, c int) int {
		if dark(r, c) {
			return 1
		}
		return 0
	}

	bits := 0
	for i := 0; i <= 5; i++ {
		bits |= bit(i, 8) << i
	}
	bits |= bit(7, 8) << 6
	bits |= bit(8, 8) << 7
	bits |= bit(8, 7) << 8
	for i := 9; i <= 14; i++ {
		bits |= bit(8, 14-i) << i
	}

	data := (bits ^ 0x5412) >> 10 // undo the format mask, drop the BCH remainder
	return data & 0b111, data >> 3
}

func firstDiff(got []string, want [][]bool) (row, col int, equal bool) {
	for r := range want {
		for c := range want[r] {
			set := got[r][c] == '1'
			if set != want[r][c] {
				return r, c, false
			}
		}
	}
	return 0, 0, true
}

func decodeDataURL(u string) (string, error) {
	_, encoded, ok := strings.Cut(u, ",")
	if !ok {
		return "", fmt.Errorf("no comma in data URL")
	}
	// PathUnescape, not QueryUnescape: encodeURIComponent writes a space as
	// %20 and leaves '+' alone, so '+' must not be read back as a space.
	return url.PathUnescape(encoded)
}
