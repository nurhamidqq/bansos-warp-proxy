package warp

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"sync"
	"time"
)

const DefaultPoolSize = 2

// egressCooldown keeps a public /64 (or IPv4 /32) that the upstream has
// rejected out of the pool for this long.
const egressCooldown = 30 * time.Minute

const refillRetryDelay = 5 * time.Second

// ErrClosed is returned when a pool is used after Close.
var ErrClosed = errors.New("WARP pool is closed")

var errDuplicateEgress = errors.New("WARP egress is already active or cooling down")

type Pool struct {
	ctx     context.Context
	cancel  context.CancelFunc
	size    int
	stack   Stack
	source  netip.Prefix
	reg     *registrar
	createMu sync.Mutex

	mu       sync.Mutex
	active   []*exit
	cooling  map[string]time.Time
	reserved map[string]bool
	next     int
	changed  chan struct{}
	closed   bool
}

// Options configures a WARP pool.
type Options struct {
	Size       int
	Stack      Stack
	SourceIPv6 netip.Prefix
}

// NewWithOptions creates a pool and registers its first exit synchronously.
// That first registration is the fail-closed gate: if Cloudflare cannot be
// reached, NewWithOptions returns an error and the caller must not serve
// traffic (which would otherwise leak the real IP via a direct fallback).
func NewWithOptions(ctx context.Context, opts Options) (*Pool, error) {
	normalized, err := ParseStack(string(opts.Stack))
	if err != nil {
		return nil, err
	}
	if opts.SourceIPv6.IsValid() && (!opts.SourceIPv6.Addr().Is6() || opts.SourceIPv6.Bits() > 64) {
		return nil, fmt.Errorf("WARP outer source must be an IPv6 prefix of /64 or shorter, got %s", opts.SourceIPv6)
	}
	size := opts.Size
	if size <= 0 {
		size = DefaultPoolSize
	}
	poolCtx, cancel := context.WithCancel(ctx)
	p := &Pool{
		ctx:      poolCtx,
		cancel:   cancel,
		size:     size,
		stack:    normalized,
		source:   opts.SourceIPv6,
		reg:      newRegistrar(),
		cooling:  make(map[string]time.Time),
		reserved: make(map[string]bool),
		changed:  make(chan struct{}, 1),
	}
	first, err := p.createExit(poolCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	p.mu.Lock()
	p.active = append(p.active, first)
	p.mu.Unlock()
	// Fill the rest of the pool in the background so a burst does not
	// wait for registrations, and so a registration rate limit does not
	// block startup after the first exit is already usable.
	go p.fill()
	return p, nil
}

func (p *Pool) notify() {
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

func randomSource(prefix netip.Prefix) (netip.Addr, error) {
	if !prefix.IsValid() {
		return netip.Addr{}, nil
	}
	addr := prefix.Addr()
	if !addr.Is6() {
		return netip.Addr{}, fmt.Errorf("source prefix is not IPv6")
	}
	b := addr.As16()
	// Keep the prefix bits, randomise the host bits.
	bits := prefix.Bits()
	// Work byte by byte: bytes fully inside the prefix stay, the
	// boundary byte is masked, the rest are random.
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return netip.Addr{}, err
	}
	for i := 0; i < 16; i++ {
		firstBit := i * 8
		lastBit := firstBit + 7
		switch {
		case lastBit < bits:
			// keep original byte
		case firstBit >= bits:
			b[i] = random[i]
		default:
			keep := bits - firstBit
			mask := byte(0xFF) << (8 - keep)
			b[i] = (b[i] & mask) | (random[i] & ^mask)
		}
	}
	out := netip.AddrFrom16(b)
	if !out.IsValid() {
		return netip.Addr{}, fmt.Errorf("could not build source address")
	}
	return out, nil
}

func (p *Pool) createExit(ctx context.Context) (*exit, error) {
	p.createMu.Lock()
	defer p.createMu.Unlock()
	reg, err := p.reg.register(ctx)
	if err != nil {
		return nil, err
	}
	var source netip.Addr
	if p.source.IsValid() {
		source, err = randomSource(p.source)
		if err != nil {
			return nil, err
		}
	}
	e, err := newExitWithStack(ctx, reg, p.stack, source)
	if err != nil {
		return nil, err
	}
	key := e.egressKey()
	p.mu.Lock()
	defer p.mu.Unlock()
	if key != "" {
		if until, ok := p.cooling[key]; ok && time.Now().Before(until) {
			e.close()
			return nil, errDuplicateEgress
		}
		if p.reserved[key] {
			e.close()
			return nil, errDuplicateEgress
		}
		p.reserved[key] = true
	}
	return e, nil
}

func (p *Pool) releaseReservation(e *exit) {
	if e == nil {
		return
	}
	p.mu.Lock()
	delete(p.reserved, e.egressKey())
	p.mu.Unlock()
}

func (p *Pool) fill() {
	for {
		p.mu.Lock()
		if p.closed || len(p.active) >= p.size {
			p.mu.Unlock()
			if p.closed {
				return
			}
			time.Sleep(2 * time.Second)
			continue
		}
		p.mu.Unlock()
		e, err := p.createExit(p.ctx)
		if err != nil {
			if errors.Is(err, errDuplicateEgress) {
				time.Sleep(refillRetryDelay)
				continue
			}
			if pause := registrationPause(err); pause > 0 {
				select {
				case <-p.ctx.Done():
					return
				case <-time.After(pause):
				}
				continue
			}
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(refillRetryDelay):
			}
			continue
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			p.releaseReservation(e)
			e.close()
			return
		}
		p.active = append(p.active, e)
		p.notify()
		p.mu.Unlock()
	}
}

// Acquire returns a lease on the next active exit (round-robin). It waits
// while the pool has no active exit, and returns ErrClosed or ctx.Err()
// when it must stop waiting.
func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrClosed
		}
		for attempts := 0; attempts < len(p.active); attempts++ {
			index := p.next % len(p.active)
			p.next = (p.next + 1) % len(p.active)
			active := p.active[index]
			if active.addRef() {
				p.mu.Unlock()
				return &Lease{pool: p, exit: active}, nil
			}
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.ctx.Done():
			return nil, ErrClosed
		case <-changed:
		}
	}
}

// Report rotates an exit when its HTTP status says the egress is blocked.
// It must be called with the response status once a relay/CONNECT attempt
// has produced one; network failures use Fail instead.
func (p *Pool) Report(lease *Lease, status int) {
	if lease == nil || lease.exit == nil {
		return
	}
	rotate, reason := lease.exit.observe(status, nil)
	if !rotate {
		return
	}
	p.penalize(lease.exit, reason)
}

// Fail drops an exit that cannot carry traffic even though the upstream
// never produced an HTTP response, such as a broken WARP handshake.
func (p *Pool) Fail(lease *Lease, reason string) {
	if lease == nil || lease.exit == nil || reason == "" {
		return
	}
	p.penalize(lease.exit, reason)
}

func (p *Pool) penalize(bad *exit, reason string) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	index := -1
	for i, e := range p.active {
		if e == bad {
			index = i
			break
		}
	}
	if index < 0 {
		p.mu.Unlock()
		return
	}
	if !bad.markPenalized() {
		p.mu.Unlock()
		return
	}
	if key := bad.egressKey(); key != "" {
		p.cooling[key] = time.Now().Add(egressCooldown)
		delete(p.reserved, key)
	}
	p.active = append(p.active[:index], p.active[index+1:]...)
	if p.next >= len(p.active) {
		p.next = 0
	}
	p.notify()
	p.mu.Unlock()
	log.Printf("warp: rotating egress %s (%s)", bad.egress.String(), reason)
	bad.retire()
	go p.fill()
}

// Snapshot reports pool capacity and active egress addresses. It never
// exposes keys or device tokens.
type Snapshot struct {
	Stack   string
	Size    int
	Active  int
	Standby int
	Cooling int
	Egress  []string
}

func (p *Pool) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap := Snapshot{Stack: string(p.stack), Size: p.size, Active: len(p.active), Cooling: len(p.cooling)}
	for _, active := range p.active {
		if active.idle() {
			snap.Standby++
		}
		if active.egress.IsValid() {
			snap.Egress = append(snap.Egress, active.egress.String())
		}
	}
	return snap
}

func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	active := append([]*exit(nil), p.active...)
	p.active = nil
	p.mu.Unlock()
	p.cancel()
	p.notify()
	for _, e := range active {
		e.retire()
		e.close()
	}
}

// Lease is one acquired WARP exit. The exit is released as soon as the
// response body is closed, so pooled connections must be closed with it.
type Lease struct {
	pool *Pool
	exit *exit
	once sync.Once
}

func (l *Lease) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if l == nil || l.exit == nil {
		return nil, errors.New("WARP lease is closed")
	}
	return l.exit.dialContext(ctx, network, address)
}

func (l *Lease) DialNetwork() string {
	if l == nil || l.exit == nil {
		return ""
	}
	return l.exit.dialNetwork()
}

func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.exit != nil {
			l.exit.release()
		}
	})
}

func (l *Lease) Egress() string {
	if l == nil || l.exit == nil {
		return ""
	}
	return l.exit.egress.String()
}
