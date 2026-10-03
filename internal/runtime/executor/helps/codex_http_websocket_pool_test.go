package helps

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type poolTestResource struct {
	closes           atomic.Int32
	started, proceed chan struct{}
	callback         func()
}

func (r *poolTestResource) Close() error {
	if r.closes.Add(1) == 1 {
		if r.started != nil {
			close(r.started)
		}
		if r.callback != nil {
			r.callback()
		}
		if r.proceed != nil {
			<-r.proceed
		}
	}
	return nil
}
func poolTestKey(credential, partition, compatibility string) CodexHTTPWebsocketKey {
	return CodexHTTPWebsocketKey{Credential: credential, Partition: partition, Compatibility: compatibility}
}

func TestCodexHTTPWebsocketPoolExclusiveReuseAndCredentialCap(t *testing.T) {
	p := NewCodexHTTPWebsocketPool()
	t.Cleanup(p.Close)
	key := poolTestKey(t.Name(), "caller", "headers")
	var leases []*CodexHTTPWebsocketLease
	for range CodexHTTPWebsocketCredentialSlots {
		lease, ok := p.Acquire(context.Background(), key)
		if !ok {
			t.Fatal("capacity refused before credential limit")
		}
		if !lease.SetResource(&poolTestResource{}) {
			t.Fatal("resource rejected")
		}
		leases = append(leases, lease)
	}
	if _, ok := p.Acquire(context.Background(), key); ok {
		t.Fatal("credential limit exceeded")
	}
	first := leases[0].Resource()
	leases[0].Release(true)
	next, ok := p.Acquire(context.Background(), key)
	if !ok || next.Resource() != first {
		t.Fatal("healthy resource was not reused")
	}
	next.Release(false)
	for _, lease := range leases[1:] {
		lease.Release(false)
	}
}

func TestCodexHTTPWebsocketPoolProcessCapIncludesOtherOwners(t *testing.T) {
	var leases []*CodexHTTPWebsocketLease
	for i := range CodexHTTPWebsocketGlobalSlots {
		p := NewCodexHTTPWebsocketPool()
		t.Cleanup(p.Close)
		key := poolTestKey(string(rune(i+100)), t.Name(), "headers")
		lease, ok := p.Acquire(context.Background(), key)
		if !ok {
			t.Fatalf("capacity refused at %d", i)
		}
		leases = append(leases, lease)
	}
	p := NewCodexHTTPWebsocketPool()
	t.Cleanup(p.Close)
	if _, ok := p.Acquire(context.Background(), poolTestKey("extra", t.Name(), "headers")); ok {
		t.Fatal("process limit exceeded")
	}
	for _, lease := range leases {
		lease.Release(false)
	}
}

func TestCodexHTTPWebsocketPoolClosingRetainsCapacityAndUnlocksCallbacks(t *testing.T) {
	p := NewCodexHTTPWebsocketPool()
	t.Cleanup(p.Close)
	key := poolTestKey(t.Name(), "caller", "headers")
	var leases []*CodexHTTPWebsocketLease
	for range CodexHTTPWebsocketCredentialSlots {
		lease, ok := p.Acquire(context.Background(), key)
		if !ok {
			t.Fatal("reserve resource")
		}
		leases = append(leases, lease)
	}
	resource := &poolTestResource{started: make(chan struct{}), proceed: make(chan struct{})}
	resource.callback = func() {
		// This acquisition proves Close callbacks do not run under the pool lock.
		if _, ok := p.Acquire(context.Background(), key); ok {
			t.Error("closing resource released capacity early")
		}
	}
	leases[0].SetResource(resource)
	done := make(chan struct{})
	go func() { leases[0].Release(false); close(done) }()
	<-resource.started
	if _, ok := p.Acquire(context.Background(), key); ok {
		t.Fatal("closing slot did not retain capacity")
	}
	close(resource.proceed)
	<-done
	lease, ok := p.Acquire(context.Background(), key)
	if !ok {
		t.Fatal("closed slot did not release capacity")
	}
	lease.Release(false)
	for _, lease := range leases[1:] {
		lease.Release(false)
	}
}

func TestCodexHTTPWebsocketPoolRotationExpiryAndShutdown(t *testing.T) {
	p := NewCodexHTTPWebsocketPool()
	t.Cleanup(p.Close)
	now := time.Unix(100, 0)
	p.now = func() time.Time { return now }
	key := poolTestKey(t.Name(), "caller", "old")
	active, _ := p.Acquire(context.Background(), key)
	activeResource := &poolTestResource{}
	active.SetResource(activeResource)
	idle, _ := p.Acquire(context.Background(), key)
	idleResource := &poolTestResource{}
	idle.SetResource(idleResource)
	idle.Release(true)
	rotated, _ := p.Acquire(context.Background(), poolTestKey(t.Name(), "caller", "new"))
	if idleResource.closes.Load() != 1 || activeResource.closes.Load() != 0 {
		t.Fatal("rotation closed the wrong lease")
	}
	active.Release(true)
	if activeResource.closes.Load() != 1 {
		t.Fatal("retired active resource survived release")
	}
	rotatedResource := &poolTestResource{}
	rotated.SetResource(rotatedResource)
	rotated.Release(true)
	now = now.Add(CodexHTTPWebsocketIdleTTL)
	p.expire(rotated.slot)
	if rotatedResource.closes.Load() != 1 {
		t.Fatal("idle resource survived expiry")
	}
	late, _ := p.Acquire(context.Background(), key)
	p.Close()
	rejected := &poolTestResource{}
	if late.SetResource(rejected) || rejected.closes.Load() != 1 {
		t.Fatal("retired generation published a resource")
	}
	late.Release(true)
	if _, ok := p.Acquire(context.Background(), key); ok {
		t.Fatal("closed pool resurrected")
	}
}

func TestCodexHTTPWebsocketPoolCancellationDoesNotReserve(t *testing.T) {
	p := NewCodexHTTPWebsocketPool()
	t.Cleanup(p.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := p.Acquire(ctx, poolTestKey(t.Name(), "caller", "headers")); ok {
		t.Fatal("canceled acquisition reserved capacity")
	}
	if len(p.slots) != 0 {
		t.Fatal("canceled acquisition leaked a slot")
	}
}

func TestCodexHTTPWebsocketPoolEpochRetiresOtherCallerScopes(t *testing.T) {
	p := NewCodexHTTPWebsocketPool()
	t.Cleanup(p.Close)
	old := poolTestKey(t.Name(), "caller-a", "headers")
	old.Epoch = "old-token"
	lease, _ := p.Acquire(context.Background(), old)
	resource := &poolTestResource{}
	lease.SetResource(resource)
	lease.Release(true)
	next := poolTestKey(t.Name(), "caller-b", "headers")
	next.Epoch = "new-token"
	replacement, ok := p.Acquire(context.Background(), next)
	if !ok || resource.closes.Load() != 1 {
		t.Fatal("credential rotation retained an incompatible caller socket")
	}
	replacement.Release(false)
}

func TestCodexHTTPWebsocketPoolFreshReservationRetainsCredentialBound(t *testing.T) {
	p := NewCodexHTTPWebsocketPool()
	t.Cleanup(p.Close)
	key := poolTestKey(t.Name(), "caller", "headers")
	var leases []*CodexHTTPWebsocketLease
	for range CodexHTTPWebsocketCredentialSlots {
		lease, ok := p.AcquireFresh(context.Background(), key)
		if !ok {
			t.Fatal("fresh reservation refused below credential bound")
		}
		lease.SetResource(&poolTestResource{})
		leases = append(leases, lease)
	}
	for _, lease := range leases[1:] {
		lease.Release(true)
	}
	if _, ok := p.AcquireFresh(context.Background(), key); ok {
		t.Fatal("fresh reservation exceeded credential bound despite idle sockets")
	}
	retired := leases[0].Resource().(*poolTestResource)
	leases[0].Release(false)
	replacement, ok := p.AcquireFresh(context.Background(), key)
	if !ok || replacement.Resource() != nil || retired.closes.Load() != 1 {
		t.Fatal("fresh reservation reused an idle resource or reserved before retirement")
	}
	replacement.Release(false)
}
