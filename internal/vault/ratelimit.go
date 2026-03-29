package vault

import (
	"net/http"
	"sync"
	"time"
)

// RateLimiter provides simple per-endpoint rate limiting.
// Tracks attempts globally (not per-IP) since this is a single-user vault.
type RateLimiter struct {
	mu       sync.Mutex
	attempts []time.Time
	window   time.Duration
	max      int
}

// NewRateLimiter creates a rate limiter allowing max attempts per window.
func NewRateLimiter(max int, window time.Duration) *RateLimiter {
	return &RateLimiter{window: window, max: max}
}

// Allow reports whether the request is within the rate limit.
func (rl *RateLimiter) Allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Prune expired entries
	valid := rl.attempts[:0]
	for _, t := range rl.attempts {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	rl.attempts = valid

	if len(rl.attempts) >= rl.max {
		return false
	}

	rl.attempts = append(rl.attempts, now)
	return true
}

// Wrap returns an HTTP middleware that rejects requests exceeding the limit.
func (rl *RateLimiter) Wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !rl.Allow() {
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded — try again later")
			return
		}
		next(w, r)
	}
}
