package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// Handler-level cover for the rules that keep the typed-pairing path end-to-end
// encrypted. The store-level tests next door check the mechanics; these check
// that the HTTP surface cannot be talked out of them.

func pairServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	s := newTestServer(t)
	room, err := s.Hub.Create()
	if err != nil {
		t.Fatal(err)
	}
	return s.Routes(stubStatic()), room.ID
}

// stubStatic is enough for Routes, which only reads index.html out of the tree.
// None of these tests touch the static handler, so this keeps the package
// independent of the real frontend.
func stubStatic() fstest.MapFS {
	return fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<!doctype html>")}}
}

func postPair(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/pair", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The rule that makes the footgun unrepresentable. A payload sealed under a
// server-chosen code is a payload the server can open, and it would look
// identical from the outside, so the combination is refused outright.
func TestMintRefusesAPayloadWithoutACode(t *testing.T) {
	h, room := pairServer(t)

	rec := postPair(t, h, `{"room":"`+room+`","payload":"c2FsdA.aXY.Y3Q"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 -- the server must not seal a payload under a code it chose", rec.Code)
	}
}

func TestMintAcceptsAClientCodeAndReturnsThePayload(t *testing.T) {
	h, room := pairServer(t)

	rec := postPair(t, h, `{"room":"`+room+`","code":"K3N8XQ2M","payload":"c2FsdA.aXY.Y3Q"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint gave %d: %s", rec.Code, rec.Body)
	}
	var minted struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatal(err)
	}
	// Echoed back in the form that goes on screen.
	if minted.Code != "K3N8-XQ2M" {
		t.Fatalf("got code %q, want K3N8-XQ2M", minted.Code)
	}

	// Redeem it the way a person would type it.
	req := httptest.NewRequest("POST", "/api/pair/k3n8-xq2m", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("redeem gave %d: %s", rec.Code, rec.Body)
	}

	var redeemed struct {
		Room    string `json:"room"`
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &redeemed); err != nil {
		t.Fatal(err)
	}
	if redeemed.Room != room {
		t.Fatalf("resolved to %q, want %q", redeemed.Room, room)
	}
	// Byte for byte, or the far device cannot unwrap it.
	if redeemed.Payload != "c2FsdA.aXY.Y3Q" {
		t.Fatalf("payload came back as %q", redeemed.Payload)
	}
}

func TestMintRejectsAMalformedCode(t *testing.T) {
	h, room := pairServer(t)

	for _, code := range []string{"k3n8xq2m", "K3N8-XQ2M", "SHORT", "K3N8XQ2I"} {
		rec := postPair(t, h, `{"room":"`+room+`","code":"`+code+`","payload":"x"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("code %q gave %d, want 400", code, rec.Code)
		}
	}
}

func TestMintConflictsOnATakenCode(t *testing.T) {
	h, room := pairServer(t)

	if rec := postPair(t, h, `{"room":"`+room+`","code":"K3N8XQ2M","payload":"a"}`); rec.Code != http.StatusCreated {
		t.Fatalf("first mint gave %d", rec.Code)
	}
	rec := postPair(t, h, `{"room":"`+room+`","code":"K3N8XQ2M","payload":"b"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second mint gave %d, want 409 so the client picks again", rec.Code)
	}
}

// No code and no payload is the pre-M3 shape, and it still works: the server
// picks, and there is no key material at stake.
func TestMintStillWorksWithNoCodeAtAll(t *testing.T) {
	h, room := pairServer(t)

	rec := postPair(t, h, `{"room":"`+room+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	var minted struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatal(err)
	}
	if len(minted.Code) != pairCodeLen+1 { // the display hyphen
		t.Fatalf("got %q, want a formatted 8-character code", minted.Code)
	}
}
