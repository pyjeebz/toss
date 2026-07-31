package api

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Token buckets, per IP. A bucket refills continuously at rate tokens/second up
// to burst, so a client can spend a burst at once and then settles to the
// steady rate -- which is what you want for pasting: several in a row is normal,
// several hundred is not.
type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   float64
	name    string
}

// newLimiter allows burst events, refilling over the given window.
func newLimiter(name string, burst int, window time.Duration) *limiter {
	return &limiter{
		buckets: make(map[string]*bucket),
		rate:    float64(burst) / window.Seconds(),
		burst:   float64(burst),
		name:    name,
	}
}

// allow spends a token, reporting whether there was one and how long until the
// next one if not.
func (l *limiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}

	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now

	if b.tokens < 1 {
		// Round up: telling someone to retry in 0s when they cannot is worse
		// than telling them 1s.
		wait := time.Duration((1-b.tokens)/l.rate*float64(time.Second)) + time.Second
		return false, wait
	}
	b.tokens--
	return true, 0
}

// sweep forgets buckets that have refilled completely. A full bucket is
// indistinguishable from one that never existed, so keeping it only costs
// memory -- and without this the map grows with every IP ever seen.
func (l *limiter) sweep(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	dropped := 0
	for key, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, key)
			dropped++
		}
	}
	return dropped
}

func (l *limiter) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// limit wraps a handler in a per-IP budget.
func (s *Server) limit(l *limiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		ok, wait := l.allow(ip, time.Now())
		if !ok {
			// Metadata only, and the IP is what is being limited so it is the
			// one thing worth recording.
			s.Log.Warn("rate limited", "limiter", l.name, "ip", ip)
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
			writeErr(w, http.StatusTooManyRequests, "slow down")
			return
		}
		next(w, r)
	}
}

// clientIP identifies who to bill a request to.
//
// Proxy headers are trusted ONLY when TOSS_TRUST_PROXY is set, because anyone
// can send an X-Forwarded-For. Trusting it by default would turn every limit
// here into a suggestion: one header per request and the budget is per-header
// rather than per-caller.
func clientIP(r *http.Request) string {
	if os.Getenv("TOSS_TRUST_PROXY") != "" {
		if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
			return v
		}
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			// Proxies append, so the LAST entry is the one our trusted proxy
			// added. Earlier entries are whatever the client felt like sending.
			parts := strings.Split(v, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
