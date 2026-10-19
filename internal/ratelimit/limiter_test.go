package ratelimit

import (
	"sync"
	"testing"
	"time"
)

func TestTokenBucketLimiter_AllowAndExceed(t *testing.T) {
	// 5 RPS, burst of 2
	limiter := NewCustom(5, 2)
	defer limiter.Close()

	key := "test-tenant-hash-1"

	// 1st request should be allowed
	allowed, wait := limiter.Allow(key)
	if !allowed || wait != 0 {
		t.Fatalf("expected 1st request to be allowed, got allowed=%v, wait=%v", allowed, wait)
	}

	// 2nd request should be allowed (burst)
	allowed, wait = limiter.Allow(key)
	if !allowed || wait != 0 {
		t.Fatalf("expected 2nd request to be allowed, got allowed=%v, wait=%v", allowed, wait)
	}

	// 3rd request immediately should be rejected
	allowed, wait = limiter.Allow(key)
	if allowed {
		t.Fatalf("expected 3rd request to be rejected, but was allowed")
	}
	if wait <= 0 {
		t.Fatalf("expected wait duration > 0, got %v", wait)
	}

	// Wait for refill (at 5 RPS, 1 token takes 200ms)
	time.Sleep(250 * time.Millisecond)

	allowed, _ = limiter.Allow(key)
	if !allowed {
		t.Fatalf("expected request after wait to be allowed")
	}
}

func TestTokenBucketLimiter_TenantIsolation(t *testing.T) {
	// 1 RPS, burst 1
	limiter := NewCustom(1, 1)
	defer limiter.Close()

	keyA := "tenant-hash-a"
	keyB := "tenant-hash-b"

	// Key A exhausts its token
	allowed, _ := limiter.Allow(keyA)
	if !allowed {
		t.Fatalf("expected keyA 1st request allowed")
	}
	allowed, _ = limiter.Allow(keyA)
	if allowed {
		t.Fatalf("expected keyA 2nd request rejected")
	}

	// Key B should NOT be affected
	allowed, _ = limiter.Allow(keyB)
	if !allowed {
		t.Fatalf("expected keyB request allowed despite keyA exhausted")
	}
}

func TestTokenBucketLimiter_ConcurrentAccess(t *testing.T) {
	limiter := NewTokenBucketLimiter(1000)
	defer limiter.Close()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := "shared-key"
			for j := 0; j < 50; j++ {
				_, _ = limiter.Allow(key)
			}
		}(i)
	}
	wg.Wait()
}
