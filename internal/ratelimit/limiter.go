package ratelimit

import (
	"math"
	"sync"
	"time"
)

// TokenBucketLimiter provides an in-process, per-key token-bucket rate limiter.
// Invariant / Security note:
// 1. Limiter state is in memory only and intentionally resets on process restart.
// 2. Keys MUST be authenticated tenant API key hashes; raw key material is never stored.
// 3. No sensitive key material is exposed.
type TokenBucketLimiter struct {
	mu      sync.RWMutex
	buckets map[string]*bucket
	rate    float64 // tokens added per second
	burst   float64 // maximum tokens bucket can hold
	stopCh  chan struct{}
}

type bucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// NewTokenBucketLimiter creates a limiter with the specified RPS and burst size equal to rps.
func NewTokenBucketLimiter(rps int) *TokenBucketLimiter {
	if rps <= 0 {
		rps = 5000
	}
	return NewCustom(float64(rps), float64(rps))
}

// NewCustom creates a limiter with specified rate and burst.
func NewCustom(rate, burst float64) *TokenBucketLimiter {
	if rate <= 0 {
		rate = 5000
	}
	if burst <= 0 {
		burst = rate
	}
	l := &TokenBucketLimiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   burst,
		stopCh:  make(chan struct{}),
	}
	go l.cleanupLoop()
	return l
}

// Close terminates background cleanup goroutines.
func (l *TokenBucketLimiter) Close() {
	select {
	case <-l.stopCh:
	default:
		close(l.stopCh)
	}
}

func (l *TokenBucketLimiter) getBucket(key string) *bucket {
	l.mu.RLock()
	b, ok := l.buckets[key]
	l.mu.RUnlock()
	if ok {
		return b
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok = l.buckets[key]
	if !ok {
		b = &bucket{
			tokens: l.burst,
			last:   time.Now(),
		}
		l.buckets[key] = b
	}
	return b
}

// Allow checks if a request with key is permitted.
// If permitted, returns (true, 0).
// If rejected, returns (false, retryAfterDuration).
func (l *TokenBucketLimiter) Allow(key string) (bool, time.Duration) {
	b := l.getBucket(key)
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.last).Seconds()
	b.last = now

	// Refill tokens based on elapsed time
	b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)

	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		return true, 0
	}

	// Calculate wait time for at least 1 token to become available
	missing := 1.0 - b.tokens
	waitSec := missing / l.rate
	waitDuration := time.Duration(math.Ceil(waitSec * float64(time.Second)))
	if waitDuration < time.Second {
		waitDuration = time.Second
	}
	return false, waitDuration
}

// cleanupLoop periodically evicts buckets that have been inactive for over 10 minutes.
func (l *TokenBucketLimiter) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-l.stopCh:
			return
		case <-ticker.C:
			l.mu.Lock()
			cutoff := time.Now().Add(-10 * time.Minute)
			for k, b := range l.buckets {
				b.mu.Lock()
				if b.last.Before(cutoff) {
					delete(l.buckets, k)
				}
				b.mu.Unlock()
			}
			l.mu.Unlock()
		}
	}
}
