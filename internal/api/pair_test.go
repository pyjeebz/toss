package api

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMintAndRedeem(t *testing.T) {
	p := newPairStore()
	code, expires, err := p.mint("room-1")
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
}

func TestClaimCarriesThePayloadBackUntouched(t *testing.T) {
	p := newPairStore()
	expires, err := p.claim("K3N8XQ2M", "room-1", "wrapped-key")
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(expires); d > pairTTL || d < pairTTL-time.Minute {
		t.Fatalf("expiry %v is not about %v away", d, pairTTL)
	}

	got, ok := p.redeem("K3N8XQ2M")
	if !ok {
		t.Fatal("claimed code did not redeem")
	}
	if got.room != "room-1" {
		t.Fatalf("resolved to %q", got.room)
	}
	// The payload is the room key wrapped under the code. It is opaque here and
	// must come back byte for byte, or the far device cannot unwrap it.
	if got.payload != "wrapped-key" {
		t.Fatalf("payload came back as %q", got.payload)
	}
}

func TestClaimRefusesATakenCode(t *testing.T) {
	p := newPairStore()
	if _, err := p.claim("K3N8XQ2M", "room-1", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.claim("K3N8XQ2M", "room-2", "b"); !errors.Is(err, errCodeTaken) {
		t.Fatalf("second claim gave %v, want errCodeTaken", err)
	}
	// The first claim must be untouched -- a loser must not overwrite a winner.
	got, ok := p.redeem("K3N8XQ2M")
	if !ok || got.room != "room-1" || got.payload != "a" {
		t.Fatalf("first claim was clobbered: %+v ok=%v", got, ok)
	}
}

func TestClaimRequiresACanonicalCode(t *testing.T) {
	// Normalisation is for what a person types back, not for what a client
	// submits: the payload is sealed under the exact string, so accepting a
	// sloppy one here would seal it under a code nobody can reproduce.
	for _, bad := range []string{
		"",
		"K3N8XQ2",   // too short
		"K3N8XQ2MM", // too long
		"k3n8xq2m",  // lower case
		"K3N8-XQ2M", // the display form
		"K3N8XQ2I",  // off-alphabet lookalike
		"K3N8XQ2U",
	} {
		p := newPairStore()
		if _, err := p.claim(bad, "room-1", "x"); !errors.Is(err, errBadCode) {
			t.Fatalf("claim(%q) gave %v, want errBadCode", bad, err)
		}
		if p.len() != 0 {
			t.Fatalf("claim(%q) was rejected but stored something anyway", bad)
		}
	}
}

func TestSingleRedemption(t *testing.T) {
	p := newPairStore()
	code, _, _ := p.mint("room-1")

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
	code, _, _ := p.mint("room-1")

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
	stale, _, _ := p.mint("room-1")

	p.mu.Lock()
	pr := p.codes[stale]
	pr.expiresAt = time.Now().Add(-time.Second)
	p.codes[stale] = pr
	p.mu.Unlock()

	if _, _, err := p.mint("room-2"); err != nil {
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
		code, _, err := p.mint("room-1")
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
	code, _, _ := p.mint("room-1")

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
		code, _, err := p.mint("room-1")
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
				code, _, err := p.mint("room-1")
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
	a, _, _ := p.mint("room-a")
	b, _, _ := p.mint("room-b")

	got, _ := p.redeem(a)
	if got.room != "room-a" {
		t.Fatalf("code for room-a resolved to %q", got.room)
	}
	got, _ = p.redeem(b)
	if got.room != "room-b" {
		t.Fatalf("code for room-b resolved to %q", got.room)
	}
}
