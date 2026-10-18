package backoff

import (
	"crypto/rand"
	"math/big"
	"time"
)

type Backoff struct {
	Base time.Duration
	Cap  time.Duration
}

func New(base, cap time.Duration) *Backoff {
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	if cap <= 0 || cap < base {
		cap = 30 * time.Second
	}
	return &Backoff{
		Base: base,
		Cap:  cap,
	}
}

// Duration calculates full jitter backoff: random(0, min(cap, base * 2^(attempt-1)))
func (b *Backoff) Duration(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	shift := attempt - 1
	if shift > 30 {
		shift = 30
	}
	multiplier := int64(1) << shift

	maxDelay := b.Base * time.Duration(multiplier)
	if maxDelay > b.Cap || maxDelay <= 0 {
		maxDelay = b.Cap
	}

	maxMillis := maxDelay.Milliseconds()
	if maxMillis <= 0 {
		return 0
	}

	n, err := rand.Int(rand.Reader, big.NewInt(maxMillis+1))
	if err != nil {
		return maxDelay / 2
	}

	return time.Duration(n.Int64()) * time.Millisecond
}
