package helps

import (
	"sync"
	"testing"
	"time"
)

func TestAntigravityThrottleBackoffLadder(t *testing.T) {
	b := NewAntigravityThrottleBackoff(5*time.Second, time.Minute)
	b.jitter = func() float64 { return 1 }
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	for i, w := range want {
		if got := b.Next("auth\x00model"); got != w {
			t.Fatalf("failure %d: delay = %s, want %s", i+1, got, w)
		}
	}
	// Other keys keep their own ladder.
	if got := b.Next("auth\x00other"); got != 5*time.Second {
		t.Fatalf("independent key delay = %s", got)
	}
	b.Reset("auth\x00model")
	if got := b.Next("auth\x00model"); got != 5*time.Second {
		t.Fatalf("delay after reset = %s, want 5s", got)
	}
}

func TestAntigravityThrottleBackoffJitterBounds(t *testing.T) {
	b := NewAntigravityThrottleBackoff(5*time.Second, time.Minute)
	b.jitter = func() float64 { return 1.19 }
	if got := b.Next("k"); got != time.Duration(float64(5*time.Second)*1.19) {
		t.Fatalf("jittered delay = %s", got)
	}
	for range 10 {
		b.Next("k")
	}
	if got := b.Next("k"); got != time.Minute {
		t.Fatalf("jitter exceeded the cap: %s", got)
	}
	b.jitter = func() float64 { return 0.8 }
	b.Reset("k")
	if got := b.Next("k"); got != 4*time.Second {
		t.Fatalf("low jitter delay = %s, want 4s", got)
	}
}

func TestAntigravityThrottleBackoffConcurrent(t *testing.T) {
	b := NewAntigravityThrottleBackoff(time.Second, time.Minute)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Next("k")
		}()
	}
	wg.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failures["k"] != 32 {
		t.Fatalf("failures = %d, want 32", b.failures["k"])
	}
}
