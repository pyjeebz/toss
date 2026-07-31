package api

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMintAndRedeem(t *testing.T) {
	p := newPairStore()
	code, expires, err := p.mint("room-1", "wrapped-key")
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != pairCodeLen {
		t.Fatalf("code %q is %d chars, want %d", code, len(code), pairCodeLen)
	}
	if d := time.Until(expires); d > pairTTL || d < pairTTL-time.Minute {
		t.Fatalf("expiry %v is not about %v away", d, pairTTL)
	}

	got, ok := p.redeem(code)
	if !ok {
		t.Fatal("fresh code did not redeem")
	}
	if got.room != "room-1" {
		t.Fatalf("resolved to %q", got.room)
	}
	// The payload is opaque and must come back untouched -- M3 puts the wrapped
	// room key in there.
	if got.payload != "wrapped-key" {
		t.Fatalf("payload came back as %q", got.payload)
	}
}

func TestSingleRedemption(t *testing.T) {
	p := newPairStore()
	code, _, _ := p.mint("room-1", "")

	if _, ok := p.redeem(code); !ok {
		t.Fatal("first redemption failed")
	}
	if _, ok := p.redeem(code); ok {
		t.Fatal("a code redeemed twice is a code that can be replayed")
	}
	if p.len() != 0 {
		t.Fatalf("%d codes left behind", p.len())
	}
}

func TestExpiredCodeIsRefused(t *testing.T) {
	p := newPairStore()
	code, _, _ := p.mint("room-1", "")

	p.mu.Lock()
	pr := p.codes[code]
	pr.expiresAt = time.Now().Add(-time.Second)
	p.codes[code] = pr
	p.mu.Unlock()

	if _, ok := p.redeem(code); ok {
		t.Fatal("expired code redeemed")
	}
}

func TestMintSweepsExpired(t *testing.T) {
	p := newPairStore()
	stale, _, _ := p.mint("room-1", "")

	p.mu.Lock()
	pr := p.codes[stale]
	pr.expiresAt = time.Now().Add(-time.Second)
	p.codes[stale] = pr
	p.mu.Unlock()

	if _, _, err := p.mint("room-2", ""); err != nil {
		t.Fatal(err)
	}
	if p.len() != 1 {
		t.Fatalf("expected the stale code swept, %d remain", p.len())
	}
}

func TestCodeAlphabetHasNoLookalikes(t *testing.T) {
	for _, bad := range []rune{'I', 'L', 'O', 'U'} {
		if strings.ContainsRune(pairAlphabet, bad) {
			t.Fatalf("%q is easy to misread and must not be in the alphabet", bad)
		}
	}
	p := newPairStore()
	for range 500 {
		code, _, err := p.mint("room-1", "")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range code {
			if !strings.ContainsRune(pairAlphabet, r) {
				t.Fatalf("code %q contains %q, which is off-alphabet", code, r)
			}
		}
	}
}

func TestNormalizeIsForgiving(t *testing.T) {
	// Everything here is the same code typed by a person in a hurry.
	for _, in := range []string{
		"K3N8XQ2M",
		"k3n8xq2m",
		"K3N8-XQ2M",
		" k3n8 - xq2m ",
		"K3N8-XQ2M\n",
	} {
		if got := normalizePairCode(in); got != "K3N8XQ2M" {
			t.Fatalf("normalize(%q) = %q", in, got)
		}
	}

	// Crockford folds the characters it excludes onto what was meant.
	if got := normalizePairCode("O1IL"); got != "0111" {
		t.Fatalf("lookalike folding gave %q, want 0111", got)
	}
}

func TestRedeemAcceptsTheDisplayedForm(t *testing.T) {
	p := newPairStore()
	code, _, _ := p.mint("room-1", "")

	// What is on screen is hyphenated; what someone types back must work.
	if _, ok := p.redeem(formatPairCode(code)); !ok {
		t.Fatalf("the displayed form %q did not redeem", formatPairCode(code))
	}
}

func TestFormatPairCode(t *testing.T) {
	if got := formatPairCode("K3N8XQ2M"); got != "K3N8-XQ2M" {
		t.Fatalf("got %q", got)
	}
	if got := formatPairCode("SHORT"); got != "SHORT" {
		t.Fatalf("unexpected length should pass through, got %q", got)
	}
}

func TestCodesAreDistinct(t *testing.T) {
	p := newPairStore()
	seen := make(map[string]bool)
	for range 2000 {
		code, _, err := p.mint("room-1", "")
		if err != nil {
			t.Fatal(err)
		}
		if seen[code] {
			t.Fatalf("minted %q twice", code)
		}
		seen[code] = true
	}
}

func TestConcurrentMintRedeem(t *testing.T) {
	p := newPairStore()
	var wg sync.WaitGroup

	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				code, _, err := p.mint("room-1", "")
				if err != nil {
					t.Error(err)
					return
				}
				p.redeem(code)
				p.len()
			}
		}()
	}
	wg.Wait()

	if p.len() != 0 {
		t.Fatalf("%d codes left over", p.len())
	}
}

// A code is only ever worth one guess, so the same code minted for two rooms
// must never cross over.
func TestCodesResolveToTheirOwnRoom(t *testing.T) {
	p := newPairStore()
	a, _, _ := p.mint("room-a", "")
	b, _, _ := p.mint("room-b", "")

	got, _ := p.redeem(a)
	if got.room != "room-a" {
		t.Fatalf("code for room-a resolved to %q", got.room)
	}
	got, _ = p.redeem(b)
	if got.room != "room-b" {
		t.Fatalf("code for room-b resolved to %q", got.room)
	}
}
