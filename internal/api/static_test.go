package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// Go's mime table has no .webmanifest entry, so without the registration in
// routes.go http.ServeContent falls through to sniffing and answers
// text/plain. Chrome refuses a manifest served as text/plain, and the only
// symptom is that the install prompt never appears.
func TestManifestIsServedAsAManifest(t *testing.T) {
	s := newTestServer(t)
	static := stubStatic()
	static["manifest.webmanifest"] = &fstest.MapFile{Data: []byte(`{"start_url":"/"}`)}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil)
	s.Routes(static).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/manifest+json") {
		t.Errorf("Content-Type = %q, want application/manifest+json", ct)
	}
}

// The manifest and icons go out through the same static handler as everything
// else, so they pick up the baseline headers. Worth pinning: an installed app
// runs from a cached-feeling shell, and losing the CSP there would be quiet.
func TestStaticAssetsKeepTheSecurityHeaders(t *testing.T) {
	s := newTestServer(t)
	static := stubStatic()
	static["manifest.webmanifest"] = &fstest.MapFile{Data: []byte(`{"start_url":"/"}`)}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil)
	s.Routes(static).ServeHTTP(rec, req)

	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("Content-Security-Policy = %q, want it to carry default-src 'self'", csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("X-Content-Type-Options is not nosniff")
	}
}

// embed.FS reports a zero ModTime, so nothing emits Last-Modified and nothing
// sets an ETag unless we do. An asset with no validator is refetched in full
// every load, and -- worse -- is cached on a browser heuristic instead of an
// instruction, which is how a device ends up on a new index.html with an old
// app.js.
func TestStaticAssetsCarryAnETagAndRevalidate(t *testing.T) {
	s := newTestServer(t)
	static := stubStatic()
	static["app.js"] = &fstest.MapFile{Data: []byte("console.log(1)")}
	h := s.Routes(static)

	for _, path := range []string{"/app.js", "/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", path, rec.Code)
		}
		if rec.Header().Get("ETag") == "" {
			t.Errorf("%s: no ETag, so the browser has nothing to revalidate against", path)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control = %q, want no-cache", path, cc)
		}
	}
}

// The point of the ETag: a repeat visit costs a 304 and no body. Without this,
// every load ships the whole frontend again.
func TestRepeatRequestGetsA304(t *testing.T) {
	s := newTestServer(t)
	static := stubStatic()
	static["app.js"] = &fstest.MapFile{Data: []byte("console.log(1)")}
	h := s.Routes(static)

	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	tag := first.Header().Get("ETag")
	if tag == "" {
		t.Fatal("no ETag on the first response")
	}

	again := httptest.NewRequest(http.MethodGet, "/app.js", nil)
	again.Header.Set("If-None-Match", tag)
	second := httptest.NewRecorder()
	h.ServeHTTP(second, again)

	if second.Code != http.StatusNotModified {
		t.Errorf("status %d on a conditional request, want 304", second.Code)
	}
	if n := second.Body.Len(); n != 0 {
		t.Errorf("304 carried %d bytes of body", n)
	}
}

// Different content must produce a different ETag, or a deploy leaves every
// browser holding the old file and revalidating happily into it.
func TestETagTracksContent(t *testing.T) {
	tagFor := func(body string) string {
		t.Helper()
		s := newTestServer(t)
		static := stubStatic()
		static["app.js"] = &fstest.MapFile{Data: []byte(body)}
		rec := httptest.NewRecorder()
		s.Routes(static).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app.js", nil))
		return rec.Header().Get("ETag")
	}

	if before, after := tagFor("console.log(1)"), tagFor("console.log(2)"); before == after {
		t.Errorf("the ETag did not change with the content: %q", before)
	}
}

// API responses must never be cached. They carry ciphertext and room state that
// moves under the client, and a backfill answered from cache is a tab showing a
// room as it used to be.
func TestAPIResponsesAreNeverCached(t *testing.T) {
	s := newTestServer(t)
	room, err := s.Hub.Create()
	if err != nil {
		t.Fatal(err)
	}
	h := s.Routes(stubStatic())

	for _, path := range []string{"/api/rooms/" + room.ID + "/items", "/healthz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", path, cc)
		}
	}
}
