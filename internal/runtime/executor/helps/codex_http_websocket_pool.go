package helps

import (
	"context"
	"io"
	"sync"
	"time"
)

const (
	CodexHTTPWebsocketGlobalSlots     = 32
	CodexHTTPWebsocketCredentialSlots = 8
	CodexHTTPWebsocketIdleTTL         = 60 * time.Second
)

// The budget includes dialing, leased, idle, and closing resources across executors.
var codexHTTPWebsocketBudget = struct {
	sync.Mutex
	total       int
	credentials map[string]int
}{credentials: make(map[string]int)}

// CodexHTTPWebsocketKey contains private compatibility fingerprints, never loggable identities.
// Partition isolates caller, credential, endpoint, and proxy. Compatibility includes token and final headers.
type CodexHTTPWebsocketKey struct{ Credential, Partition, Compatibility, Epoch string }

type codexHTTPWebsocketSlot struct {
	key                      CodexHTTPWebsocketKey
	resource                 io.Closer
	active, retired, closing bool
	idleSince                time.Time
	timer                    *time.Timer
}

// CodexHTTPWebsocketPool leases resources exclusively without a wait queue.
// Close callbacks run outside its lock. Closing resources retain capacity until Close returns.
type CodexHTTPWebsocketPool struct {
	mu     sync.Mutex
	slots  map[*codexHTTPWebsocketSlot]struct{}
	closed bool
	now    func() time.Time
}

func NewCodexHTTPWebsocketPool() *CodexHTTPWebsocketPool {
	return &CodexHTTPWebsocketPool{slots: make(map[*codexHTTPWebsocketSlot]struct{}), now: time.Now}
}

type CodexHTTPWebsocketLease struct {
	pool *CodexHTTPWebsocketPool
	slot *codexHTTPWebsocketSlot
	once sync.Once
}

// Acquire reserves capacity before a caller dials. False permits a pre-send HTTP fallback.
func (p *CodexHTTPWebsocketPool) Acquire(ctx context.Context, key CodexHTTPWebsocketKey) (*CodexHTTPWebsocketLease, bool) {
	return p.acquire(ctx, key, true)
}

// AcquireFresh reserves a new resource without borrowing another idle connection.
// The caller first closes and releases its failed pre-send lease.
func (p *CodexHTTPWebsocketPool) AcquireFresh(ctx context.Context, key CodexHTTPWebsocketKey) (*CodexHTTPWebsocketLease, bool) {
	return p.acquire(ctx, key, false)
}

func (p *CodexHTTPWebsocketPool) acquire(ctx context.Context, key CodexHTTPWebsocketKey, reuse bool) (*CodexHTTPWebsocketLease, bool) {
	if ctx.Err() != nil {
		return nil, false
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, false
	}
	var closing []*codexHTTPWebsocketSlot
	var available *codexHTTPWebsocketSlot
	now := p.now()
	for s := range p.slots {
		if s.closing {
			continue
		}
		if !s.active && now.Sub(s.idleSince) >= CodexHTTPWebsocketIdleTTL {
			s.retired = true
		}
		if (s.key.Partition == key.Partition && s.key.Compatibility != key.Compatibility) || (s.key.Credential == key.Credential && s.key.Epoch != key.Epoch) {
			s.retired = true
		}
		if s.retired && !s.active {
			s.closing = true
			if s.timer != nil {
				s.timer.Stop()
			}
			closing = append(closing, s)
		} else if reuse && !s.active && !s.retired && s.key == key {
			available = s
		}
	}
	if available != nil {
		available.active = true
		if available.timer != nil {
			available.timer.Stop()
		}
	}
	if available == nil {
		codexHTTPWebsocketBudget.Lock()
		if codexHTTPWebsocketBudget.total < CodexHTTPWebsocketGlobalSlots && codexHTTPWebsocketBudget.credentials[key.Credential] < CodexHTTPWebsocketCredentialSlots {
			codexHTTPWebsocketBudget.total++
			codexHTTPWebsocketBudget.credentials[key.Credential]++
			available = &codexHTTPWebsocketSlot{key: key, active: true}
			p.slots[available] = struct{}{}
		}
		codexHTTPWebsocketBudget.Unlock()
	}
	p.mu.Unlock()
	for _, s := range closing {
		p.finishClose(s)
	}
	if available == nil {
		return nil, false
	}
	return &CodexHTTPWebsocketLease{pool: p, slot: available}, true
}

// Resource remains exclusively borrowed until Release. A retired lease cannot publish a new resource.
func (l *CodexHTTPWebsocketLease) Resource() io.Closer {
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	return l.slot.resource
}
func (l *CodexHTTPWebsocketLease) SetResource(resource io.Closer) bool {
	p := l.pool
	p.mu.Lock()
	accepted := !p.closed && !l.slot.retired && !l.slot.closing && l.slot.resource == nil
	if accepted {
		l.slot.resource = resource
	}
	p.mu.Unlock()
	if !accepted && resource != nil {
		_ = resource.Close()
	}
	return accepted
}

// Release returns only fully cleaned, healthy resources. All other resources close before capacity returns.
func (l *CodexHTTPWebsocketLease) Release(healthy bool) {
	l.once.Do(func() {
		p, s := l.pool, l.slot
		p.mu.Lock()
		s.active = false
		if !healthy || s.resource == nil || s.retired || p.closed {
			s.retired = true
			if s.closing {
				p.mu.Unlock()
				return
			}
			s.closing = true
			p.mu.Unlock()
			p.finishClose(s)
			return
		}
		s.idleSince = p.now()
		s.timer = time.AfterFunc(CodexHTTPWebsocketIdleTTL, func() { p.expire(s) })
		p.mu.Unlock()
	})
}

func (p *CodexHTTPWebsocketPool) expire(s *codexHTTPWebsocketSlot) {
	p.mu.Lock()
	if _, ok := p.slots[s]; !ok || s.active || s.closing || p.now().Sub(s.idleSince) < CodexHTTPWebsocketIdleTTL {
		p.mu.Unlock()
		return
	}
	s.retired, s.closing = true, true
	p.mu.Unlock()
	p.finishClose(s)
}

func (p *CodexHTTPWebsocketPool) finishClose(s *codexHTTPWebsocketSlot) {
	if s.resource != nil {
		_ = s.resource.Close()
	}
	p.mu.Lock()
	delete(p.slots, s)
	codexHTTPWebsocketBudget.Lock()
	codexHTTPWebsocketBudget.total--
	codexHTTPWebsocketBudget.credentials[s.key.Credential]--
	if codexHTTPWebsocketBudget.credentials[s.key.Credential] == 0 {
		delete(codexHTTPWebsocketBudget.credentials, s.key.Credential)
	}
	codexHTTPWebsocketBudget.Unlock()
	p.mu.Unlock()
}

// Close retires the generation permanently and interrupts all published resources, including active leases.
func (p *CodexHTTPWebsocketPool) Close() {
	p.mu.Lock()
	p.closed = true
	var closing []*codexHTTPWebsocketSlot
	for s := range p.slots {
		s.retired = true
		if s.timer != nil {
			s.timer.Stop()
		}
		// Dialing slots remain reserved until their owner publishes or releases them.
		if !s.closing && s.resource != nil {
			s.closing = true
			closing = append(closing, s)
		}
	}
	p.mu.Unlock()
	for _, s := range closing {
		p.finishClose(s)
	}
}
