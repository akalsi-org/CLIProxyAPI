package helps

import (
	"math/rand/v2"
	"sync"
	"time"
)

// AntigravityThrottleBackoff computes cooldowns for consecutive Antigravity
// throttling responses that carry no provider retry guidance. Each key
// (credential and model) backs off exponentially until a success resets it.
// It is safe for concurrent use.
type AntigravityThrottleBackoff struct {
	mu       sync.Mutex
	failures map[string]int
	base     time.Duration
	max      time.Duration
	// jitter returns a factor in [0.8, 1.2). Tests replace it.
	jitter func() float64
}

// NewAntigravityThrottleBackoff returns a ladder that starts at base and caps at max.
func NewAntigravityThrottleBackoff(base, max time.Duration) *AntigravityThrottleBackoff {
	return &AntigravityThrottleBackoff{
		failures: make(map[string]int),
		base:     base,
		max:      max,
		jitter:   func() float64 { return 0.8 + 0.4*rand.Float64() },
	}
}

// Next records one more consecutive failure for key and returns its cooldown.
// Jitter spreads concurrent retries; the result never exceeds max.
func (b *AntigravityThrottleBackoff) Next(key string) time.Duration {
	b.mu.Lock()
	b.failures[key]++
	n := b.failures[key]
	jitter := b.jitter()
	b.mu.Unlock()

	delay := b.base
	for i := 1; i < n && delay < b.max; i++ {
		delay *= 2
	}
	delay = time.Duration(float64(min(delay, b.max)) * jitter)
	return min(delay, b.max)
}

// Reset clears the failure count for key after a success.
func (b *AntigravityThrottleBackoff) Reset(key string) {
	b.mu.Lock()
	delete(b.failures, key)
	b.mu.Unlock()
}
