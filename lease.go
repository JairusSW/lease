// Package lease provides a pool of stateless WebAssembly instances that callers
// borrow for a single unit of work and return.
//
// The model is the serverless / FaaS execution pattern: keep a set of warm,
// clean instances; Acquire one, Invoke one or more exported functions on it, then
// Release it. On release the instance is reset to clean state and its capacity
// returns to the pool. Instances run in parallel — one lease at a time per
// instance, so a pool of N instances serves N concurrent calls.
//
// Instances are minted by a Factory. The common case is FromSnapshot, which
// restores fresh instances from a wago.Snapshot without re-running the module's
// init or start function — a warm start rather than a cold one. Because WebAssembly
// has no in-place "reset instance" operation, a reset is realized by closing the
// used instance and minting a clean replacement from the snapshot; each restore
// reproduces the identical captured image, so every lease starts from the same
// clean state.
//
// The pool is a plain library (Pool/NewPool). A thin wago.Extension wrapper
// (New/Plugin) is also provided so it can be registered on a runtime and shared
// with other plugins through ServiceKey.
package lease

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wago-org/wago"
)

// Factory mints a fresh, clean instance. It is called to grow the pool and to
// produce the clean replacement that a reset-on-release installs. It must be safe
// for concurrent use. The canonical implementation is FromSnapshot.
type Factory func() (*wago.Instance, error)

// FromSnapshot returns a Factory that restores fresh instances from snap. The
// snapshot supplies the restored instances' imports and GC config (captured at
// Capture time), so the pool needs nothing else — no runtime, no host wiring —
// for a module whose imports were present at capture.
func FromSnapshot(snap *wago.Snapshot) Factory {
	return func() (*wago.Instance, error) {
		if snap == nil {
			return nil, errors.New("lease: nil snapshot")
		}
		return wago.Instantiate(snap)
	}
}

// ReleasePolicy selects what Release does with a borrowed instance.
type ReleasePolicy uint8

const (
	// Reset discards the used instance and mints a clean replacement, so every
	// lease starts from clean state. This is the default (zero value).
	Reset ReleasePolicy = iota
	// Reuse returns the same instance to the pool without resetting it. Faster, but
	// only safe when the exported functions are truly stateless (leave no state
	// behind in memory or globals); the caller is responsible for that.
	Reuse
)

// OptimalInstances returns the recommended MaxLive for CPU-bound stateless work
// on this machine: GOMAXPROCS, at least 1. Running more compute-bound instances
// than the runtime can execute in parallel adds no throughput, only context
// switching and cache pressure. Pools left with MaxLive == 0 use this. Raise it
// explicitly for I/O-bound handlers that spend time waiting.
func OptimalInstances() int {
	if n := runtime.GOMAXPROCS(0); n > 0 {
		return n
	}
	return 1
}

// Errors returned by the pool. Match them with errors.Is.
var (
	ErrPoolClosed     = errors.New("lease: pool is closed")
	ErrAcquireTimeout = errors.New("lease: acquire timed out")
	ErrLeaseReleased  = errors.New("lease: lease already released")
	ErrNoFactory      = errors.New("lease: no factory or snapshot configured")
)

// Options configures a Pool.
type Options struct {
	// MaxLive caps the total instances (idle + leased). Acquire past it blocks
	// until a lease is returned (or the context/AcquireTimeout fires). Zero takes
	// OptimalInstances().
	MaxLive int
	// MinIdle pre-warms this many clean instances at construction so the first
	// acquisitions are warm. Must not exceed MaxLive. Default 0 (mint on demand).
	MinIdle int
	// Release selects reset (default) vs reuse on Release. See ReleasePolicy.
	Release ReleasePolicy
	// AcquireTimeout bounds how long Acquire waits for a free instance when the
	// pool is at capacity. Zero waits until one frees up or the context is done.
	AcquireTimeout time.Duration
}

// Pool is a bounded, self-warming pool of instances leased one at a time.
type Pool struct {
	factory Factory
	max     int
	minIdle int
	policy  ReleasePolicy
	waitFor time.Duration
	ready   chan *wago.Instance
	done    chan struct{}

	mu       sync.Mutex
	live     int // instances that exist: idle (in ready) + leased
	leased   int // currently checked out
	closed   bool
	acquired uint64
	released uint64
	minted   uint64
	failed   uint64
	waited   uint64
}

// NewPool creates a pool that mints instances with factory. It pre-warms MinIdle
// instances; if any pre-warm mint fails, the pool is closed and the error
// returned.
func NewPool(factory Factory, opts Options) (*Pool, error) {
	if factory == nil {
		return nil, ErrNoFactory
	}
	max := opts.MaxLive
	if max <= 0 {
		max = OptimalInstances()
	}
	if opts.MinIdle < 0 || opts.MinIdle > max {
		return nil, fmt.Errorf("lease: MinIdle %d must be in [0, MaxLive=%d]", opts.MinIdle, max)
	}
	p := &Pool{
		factory: factory, max: max, minIdle: opts.MinIdle, policy: opts.Release,
		waitFor: opts.AcquireTimeout, ready: make(chan *wago.Instance, max), done: make(chan struct{}),
	}
	for i := 0; i < opts.MinIdle; i++ {
		inst, err := factory()
		if err != nil {
			_ = p.Close()
			return nil, fmt.Errorf("lease: pre-warm instance %d: %w", i, err)
		}
		p.mu.Lock()
		p.live++
		p.minted++
		p.mu.Unlock()
		p.ready <- inst
	}
	return p, nil
}

// Acquire checks out a clean instance, blocking until one is available, the
// context is cancelled, or AcquireTimeout elapses. The returned Lease must be
// Released.
func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Warm path: an idle instance is waiting.
	select {
	case inst := <-p.ready:
		return p.checkout(inst), nil
	default:
	}
	// Mint path: grow the pool if under capacity.
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}
	if p.live < p.max {
		p.live++ // reserve the slot before minting so concurrent acquires respect MaxLive
		p.mu.Unlock()
		inst, err := p.factory()
		if err != nil {
			p.mu.Lock()
			p.live--
			p.failed++
			p.mu.Unlock()
			return nil, fmt.Errorf("lease: mint: %w", err)
		}
		p.mu.Lock()
		p.minted++
		p.mu.Unlock()
		return p.checkout(inst), nil
	}
	p.waited++
	p.mu.Unlock()
	// Wait path: at capacity — wait for a Release, cancellation, or timeout.
	var timeout <-chan time.Time
	if p.waitFor > 0 {
		t := time.NewTimer(p.waitFor)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case inst := <-p.ready:
		return p.checkout(inst), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.done:
		return nil, ErrPoolClosed
	case <-timeout:
		return nil, ErrAcquireTimeout
	}
}

func (p *Pool) checkout(inst *wago.Instance) *Lease {
	p.mu.Lock()
	p.leased++
	p.acquired++
	p.mu.Unlock()
	return &Lease{pool: p, inst: inst}
}

// release returns a borrowed instance to the pool, applying the release policy.
func (p *Pool) release(inst *wago.Instance) {
	p.mu.Lock()
	p.leased--
	p.released++
	closed := p.closed
	policy := p.policy
	p.mu.Unlock()

	if policy == Reuse && !closed {
		if !p.offer(inst) {
			p.discard(inst)
		}
		return
	}
	// Reset: discard the used instance and install a clean replacement in its slot.
	_ = inst.Close()
	if closed {
		p.dropSlot()
		return
	}
	fresh, err := p.factory()
	if err != nil {
		p.mu.Lock()
		p.live--
		p.failed++
		p.mu.Unlock()
		return
	}
	p.mu.Lock()
	p.minted++
	p.mu.Unlock()
	if !p.offer(fresh) {
		p.discard(fresh)
	}
}

// offer places a clean instance on the ready channel. It never blocks: correct
// accounting guarantees room, and the false return is a safety valve.
func (p *Pool) offer(inst *wago.Instance) bool {
	select {
	case p.ready <- inst:
		return true
	default:
		return false
	}
}

// discard closes an instance that could not be returned and frees its slot.
func (p *Pool) discard(inst *wago.Instance) {
	_ = inst.Close()
	p.dropSlot()
}

func (p *Pool) dropSlot() {
	p.mu.Lock()
	if p.live > 0 {
		p.live--
	}
	p.mu.Unlock()
}

// Close closes every idle instance and stops the pool from issuing new leases.
// Instances still leased out are closed when their Release is called. Close is
// idempotent.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	close(p.done)
	p.mu.Unlock()
	for {
		select {
		case inst := <-p.ready:
			_ = inst.Close()
			p.dropSlot()
		default:
			return nil
		}
	}
}

// Stats is an atomic snapshot of the pool.
type Stats struct {
	Live    int // instances that exist (idle + leased)
	Idle    int // clean instances ready to lease
	Leased  int // currently checked out
	Max     int
	MinIdle int
	Closed  bool

	Acquired uint64 // leases handed out
	Released uint64 // leases returned
	Minted   uint64 // instances created by the factory
	Failed   uint64 // factory errors
	Waited   uint64 // acquisitions that had to block at capacity
}

// Stats returns a snapshot of the pool's size and counters.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	idle := p.live - p.leased
	if idle < 0 {
		idle = 0
	}
	return Stats{
		Live: p.live, Idle: idle, Leased: p.leased, Max: p.max, MinIdle: p.minIdle, Closed: p.closed,
		Acquired: p.acquired, Released: p.released, Minted: p.minted, Failed: p.failed, Waited: p.waited,
	}
}

// Lease is a checked-out instance. Invoke exported functions on it, then Release
// it. A Lease is not safe for concurrent use by multiple goroutines.
type Lease struct {
	pool *Pool
	inst *wago.Instance
	done atomic.Bool
}

// Invoke calls an exported function on the leased instance. The returned slots are
// valid until the next call on this instance; copy them if you retain them past
// the next Invoke.
func (l *Lease) Invoke(export string, args ...uint64) ([]uint64, error) {
	if l.done.Load() {
		return nil, ErrLeaseReleased
	}
	return l.inst.Invoke(export, args...)
}

// Instance exposes the underlying instance for calls the Lease does not wrap
// (e.g. Instance.Call with a context). Do not close it or use it after Release.
func (l *Lease) Instance() *wago.Instance {
	if l.done.Load() {
		return nil
	}
	return l.inst
}

// Release returns the instance to the pool (resetting it per the pool's
// ReleasePolicy). It is idempotent; further Invoke calls fail with
// ErrLeaseReleased.
func (l *Lease) Release() {
	if !l.done.CompareAndSwap(false, true) {
		return
	}
	inst := l.inst
	l.inst = nil
	l.pool.release(inst)
}
