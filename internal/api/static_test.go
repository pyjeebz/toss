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
