package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// rule returns the body of the first top-level CSS rule with this exact
// selector, so a test can assert on one block rather than the whole file.
func rule(t *testing.T, css, selector string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(selector) + `\s*\{([^}]*)\}`)
	m := re.FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("no rule for %q in app.css", selector)
	}
	return m[1]
}

// The pairing sheet must be able to scroll.
//
// It is position: fixed, so with overflow visible anything past the viewport is
// not merely off screen, it is unreachable -- no scrollbar, no gesture, nothing.
// That is how the reset button disappeared: the sheet holds a 12rem QR, a code,
// a countdown, a join form and the reset, which lands within a few rem of a
// phone's usable height, and adding one section pushed the bottom off the end.
//
// The failure is silent from every angle. The markup is right, the handler is
// bound, the click just never happens because the pixels are not there.
func TestThePairingSheetCanScroll(t *testing.T) {
	raw, err := fs.ReadFile(FS(), "app.css")
	if err != nil {
		t.Fatalf("app.css is not embedded: %v", err)
	}
	sheet := rule(t, string(raw), ".sheet")

	if strings.Contains(sheet, "overflow: visible") {
		t.Error(".sheet is overflow: visible; content past the viewport cannot be reached")
	}
	if !strings.Contains(sheet, "overflow-y: auto") {
		t.Error(".sheet needs overflow-y: auto so a tall sheet can be scrolled")
	}
	if !strings.Contains(sheet, "max-height") {
		t.Error(".sheet needs a max-height, or there is nothing for the scroll to bite on")
	}
	// Scrolling clips both axes, and .sheet-body::before paints outside its box
	// in both. Without padding the paper's rough edge is sliced off square.
	if !strings.Contains(sheet, "padding:") {
		t.Error(".sheet needs padding to keep the rough edge inside the scroll box")
	}
	// The scrollbar is hidden for looks. That is only safe while the scrolling
	// itself survives, which is what the assertions above are for -- the two
	// belong together, and this is here so removing one draws attention to the
	// other.
	if !strings.Contains(sheet, "scrollbar-width: none") {
		t.Error(".sheet should hide the scrollbar; the design cannot absorb a gutter")
	}
	if !strings.Contains(string(raw), ".sheet::-webkit-scrollbar") {
		t.Error("no ::-webkit-scrollbar rule, so Chrome and Safari still draw one")
	}
}

// The note explaining what "start a new room" destroys has to follow the button
// in the DOM: it is revealed by an adjacent sibling selector when the button
// arms, and reordering them hides the warning exactly when it is needed.
func TestTheResetWarningFollowsItsButton(t *testing.T) {
	raw, err := fs.ReadFile(FS(), "index.html")
	if err != nil {
		t.Fatalf("index.html is not embedded: %v", err)
	}
	html := string(raw)

	button := strings.Index(html, `data-role="rotate"`)
	note := strings.Index(html, `class="pair-reset-note"`)
	if button < 0 || note < 0 {
		t.Fatal("the reset button or its note is missing from index.html")
	}
	if note < button {
		t.Error("the note comes before the button, so the sibling selector never matches it")
	}

	css, err := fs.ReadFile(FS(), "app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), ".action-reset[data-confirming] + .pair-reset-note") {
		t.Error("nothing reveals the note when the button arms")
	}
}
