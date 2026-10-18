package backoff

import (
	"testing"
	"time"
)

func TestBackoffFullJitterBounds(t *testing.T) {
	base := 50 * time.Millisecond
	cap := 5 * time.Second
	b := New(base, cap)

	// A5.1: 1,000 sampled delays stay within bounds and are not all equal
	samples := 1000
	seenValues := make(map[time.Duration]bool)

	for attempt := 1; attempt <= 10; attempt++ {
		for i := 0; i < samples; i++ {
			d := b.Duration(attempt)
			if d < 0 {
				t.Fatalf("Delay cannot be negative: %v", d)
			}
			if d > cap {
				t.Fatalf("Delay exceeded cap %v: %v", cap, d)
			}
			seenValues[d] = true
		}
	}

	if len(seenValues) < 50 {
		t.Fatalf("A5.1 violation: delays are not sufficiently random/jittered, unique count: %d", len(seenValues))
	}
}
