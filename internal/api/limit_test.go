package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestBurstThenRefill(t *testing.T) {
	l := newLimiter("test", 3, time.Minute)
	now := time.Now()

	for i := range 3 {
		if ok, _ := l.allow("ip", now); !ok {
			t.Fatalf("request %d refused inside the burst", i+1)
		}
	}
	ok, wait := l.allow("ip", now)
	if ok {
		t.Fatal("burst was not enforced")
	}
	if wait <= 0 {
		t.Fatal("a refusal must say how long to wait")
	}

	// One token every 20s at 3/minute.
	if ok, _ := l.allow("ip", now.Add(21*time.Second)); !ok {
		t.Fatal("bucket did not refill")
	}
}

func TestBucketsAreIndependentPerIP(t *testing.T) {
	l := newLimiter("test", 1, time.Minute)
	now := time.Now()

	if ok, _ := l.allow("10.0.0.1", now); !ok {
		t.Fatal("first IP refused")
	}
	if ok, _ := l.allow("10.0.0.1", now); ok {
		t.Fatal("first IP not limited")
	}
	if ok, _ := l.allow("10.0.0.2", now); !ok {
		t.Fatal("one IP's spending must not come out of another's budget")
	}
}

func TestRefillIsCappedAtBurst(t *testing.T) {
	l := newLimiter("test", 5, time.Minute)
	now := time.Now()

	l.allow("ip", now)
	// A week of idleness must not bank a week of requests.
	for i := range 5 {
		if ok, _ := l.allow("ip", now.Add(7*24*time.Hour)); !ok {
			t.Fatalf("request %d refused after a long idle", i+1)
		}
	}
	if ok, _ := l.allow("ip", now.Add(7*24*time.Hour)); ok {
		t.Fatal("idling banked more than the burst")
	}
}

func TestSweepForgetsFullBuckets(t *testing.T) {
	l := newLimiter("test", 2, time.Minute)
	now := time.Now()

	l.allow("spent", now)
	l.allow("spent", now)
	l.allow("partial", now)
	if l.len() != 2 {
		t.Fatalf("expected 2 buckets, got %d", l.len())
	}

	// Nothing has refilled yet.
	l.sweep(now)
	if l.len() != 2 {
		t.Fatalf("swept buckets that still hold state, %d left", l.len())
	}

	// Well past a full refill: both are indistinguishable from never-seen.
	l.sweep(now.Add(2 * time.Minute))
	if l.len() != 0 {
		t.Fatalf("expected the map emptied, %d left", l.len())
	}
}

func TestLimitMiddlewareReturns429(t *testing.T) {
	s := newTestServer(t)
	l := newLimiter("test", 1, time.Minute)
	h := s.limit(l, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	req := httptest.NewRequest("POST", "/whatever", nil)
	req.RemoteAddr = "10.0.0.9:1234"

	first := httptest.NewRecorder()
	h(first, req)
	if first.Code != http.StatusOK {
		t.Fatalf("first request got %d", first.Code)
	}

	second := httptest.NewRecorder()
	h(second, req)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request got %d, want 429", second.Code)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
}

// The whole point of a per-IP limit is that it is per caller. If a spoofable
// header were trusted by default, one header per request would defeat it.
func TestForwardedHeadersIgnoredByDefault(t *testing.T) {
	t.Setenv("TOSS_TRUST_PROXY", "")

	req := httptest.NewRequest("POST", "/", nil)
	req.RemoteAddr = "10.0.0.1:5000"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Real-IP", "5.6.7.8")

	if got := clientIP(req); got != "10.0.0.1" {
		t.Fatalf("clientIP = %q; a spoofed header must not win", got)
	}
}

func TestForwardedHeadersUsedWhenTrusted(t *testing.T) {
	t.Setenv("TOSS_TRUST_PROXY", "1")

	req := httptest.NewRequest("POST", "/", nil)
	req.RemoteAddr = "10.0.0.1:5000"
	req.Header.Set("X-Real-IP", "5.6.7.8")
	if got := clientIP(req); got != "5.6.7.8" {
		t.Fatalf("clientIP = %q, want the X-Real-IP value", got)
	}

	req.Header.Del("X-Real-IP")
	// A client can prepend anything; our proxy appends the truth at the end.
	req.Header.Set("X-Forwarded-For", "9.9.9.9, 1.2.3.4")
	if got := clientIP(req); got != "1.2.3.4" {
		t.Fatalf("clientIP = %q, want the last hop", got)
	}
}

func TestIPv6RemoteAddr(t *testing.T) {
	req := httptest.NewRequest("POST", "/", nil)
	req.RemoteAddr = "[2001:db8::1]:4321"
	if got := clientIP(req); got != "2001:db8::1" {
		t.Fatalf("clientIP = %q", got)
	}
}

func TestConcurrentAllow(t *testing.T) {
	l := newLimiter("test", 1000, time.Minute)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			now := time.Now()
			for range 50 {
				l.allow("shared", now)
				l.allow("other", now)
				l.len()
			}
		}()
	}
	wg.Wait()
}
