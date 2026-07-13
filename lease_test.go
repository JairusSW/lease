package lease

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wago-org/wago"
	"github.com/wago-org/wago/testutil/wasmtest"
)

// counterModule hand-builds a wasm module with a mutable global "counter" and a
// linear memory, exporting bump(n)->i32 (adds n, returns total) and get()->i32.
// Enough mutable state to prove reset returns instances to clean state.
func counterModule() []byte {
	typeBump := []byte{0x60, 0x01, 0x7f, 0x01, 0x7f} // (i32) -> (i32)
	typeGet := []byte{0x60, 0x00, 0x01, 0x7f}        // () -> (i32)
	types := wasmtest.Vec(typeBump, typeGet)

	funcs := wasmtest.Vec(wasmtest.ULEB(0), wasmtest.ULEB(1))     // func0:bump, func1:get
	memory := wasmtest.Vec([]byte{0x00, 0x01})                    // one page, min only
	globals := wasmtest.Vec([]byte{0x7f, 0x01, 0x41, 0x00, 0x0b}) // mut i32 = 0

	exports := wasmtest.Vec(
		wasmtest.ExportEntry("bump", 0x00, 0),
		wasmtest.ExportEntry("get", 0x00, 1),
		wasmtest.ExportEntry("counter", 0x03, 0),
		wasmtest.ExportEntry("mem", 0x02, 0),
	)

	// bump: counter += n; return counter.
	bump := wasmtest.Code([]byte{0x23, 0x00, 0x20, 0x00, 0x6a, 0x24, 0x00, 0x23, 0x00, 0x0b})
	// get: return counter.
	get := wasmtest.Code([]byte{0x23, 0x00, 0x0b})
	code := wasmtest.Vec(bump, get)

	return wasmtest.Module(
		wasmtest.Section(1, types),
		wasmtest.Section(3, funcs),
		wasmtest.Section(5, memory),
		wasmtest.Section(6, globals),
		wasmtest.Section(7, exports),
		wasmtest.Section(10, code),
	)
}

// compiledFactory returns a Factory that mints fresh instances from a plainly
// compiled counter module. Each fresh instance starts at counter == 0, so it
// exercises the pool's reset semantics without snapshots.
func compiledFactory(t *testing.T) Factory {
	t.Helper()
	c, err := wago.Compile(nil, counterModule())
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return func() (*wago.Instance, error) { return wago.Instantiate(c) }
}

func acquire(t *testing.T, p *Pool) *Lease {
	t.Helper()
	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	return l
}

func bump(t *testing.T, l *Lease, n uint64) uint64 {
	t.Helper()
	out, err := l.Invoke("bump", n)
	if err != nil {
		t.Fatalf("bump: %v", err)
	}
	return out[0]
}

func get(t *testing.T, l *Lease) uint64 {
	t.Helper()
	out, err := l.Invoke("get")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return out[0]
}

func TestOptimalInstances(t *testing.T) {
	if OptimalInstances() < 1 {
		t.Fatal("OptimalInstances() < 1")
	}
}

func TestPoolBasicRoundTrip(t *testing.T) {
	p, err := NewPool(compiledFactory(t), Options{MaxLive: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	l := acquire(t, p)
	if got := bump(t, l, 7); got != 7 {
		t.Fatalf("bump(7) = %d, want 7", got)
	}
	if got := bump(t, l, 3); got != 10 {
		t.Fatalf("bump(3) = %d, want 10", got)
	}
	l.Release()

	// Invoking after release fails; release is idempotent.
	if _, err := l.Invoke("get"); !errors.Is(err, ErrLeaseReleased) {
		t.Fatalf("invoke after release = %v, want ErrLeaseReleased", err)
	}
	l.Release() // no panic
}

func TestPoolResetGivesCleanState(t *testing.T) {
	p, err := NewPool(compiledFactory(t), Options{MaxLive: 1}) // one instance, forced reuse of the slot
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	l := acquire(t, p)
	bump(t, l, 42)
	l.Release() // reset: the dirty instance is discarded, a clean one installed

	l = acquire(t, p)
	if got := get(t, l); got != 0 {
		t.Fatalf("state after reset = %d, want 0 (clean)", got)
	}
	l.Release()
}

func TestPoolReusePolicyKeepsState(t *testing.T) {
	p, err := NewPool(compiledFactory(t), Options{MaxLive: 1, Release: Reuse})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	l := acquire(t, p)
	bump(t, l, 42)
	l.Release() // reuse: same instance returns without reset

	l = acquire(t, p)
	if got := get(t, l); got != 42 {
		t.Fatalf("state after reuse = %d, want 42 (preserved)", got)
	}
	l.Release()
}

func TestPoolMinIdlePrewarm(t *testing.T) {
	p, err := NewPool(compiledFactory(t), Options{MaxLive: 8, MinIdle: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	st := p.Stats()
	if st.Idle != 3 || st.Live != 3 || st.Minted != 3 {
		t.Fatalf("prewarm stats = %+v, want Idle/Live/Minted = 3", st)
	}
}

func TestPoolMaxLiveBlocksAndTimeout(t *testing.T) {
	p, err := NewPool(compiledFactory(t), Options{MaxLive: 2, AcquireTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	a := acquire(t, p)
	b := acquire(t, p)
	if st := p.Stats(); st.Leased != 2 || st.Live != 2 {
		t.Fatalf("stats at capacity = %+v", st)
	}
	// Third acquire has no capacity and times out.
	if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrAcquireTimeout) {
		t.Fatalf("acquire at capacity = %v, want ErrAcquireTimeout", err)
	}
	// Releasing frees a slot for the next acquire.
	a.Release()
	c, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	c.Release()
	b.Release()
}

func TestPoolAcquireContextCancel(t *testing.T) {
	p, err := NewPool(compiledFactory(t), Options{MaxLive: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	held := acquire(t, p) // pool now at capacity
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := p.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire = %v, want context.Canceled", err)
	}
	held.Release()
}

func TestPoolCloseReleasesAndRejects(t *testing.T) {
	p, err := NewPool(compiledFactory(t), Options{MaxLive: 4, MinIdle: 2})
	if err != nil {
		t.Fatal(err)
	}
	held := acquire(t, p)
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Idle instances were closed; further acquires are rejected.
	if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("acquire after close = %v, want ErrPoolClosed", err)
	}
	held.Release() // closes the still-leased instance
	_ = p.Close()  // idempotent
	if st := p.Stats(); !st.Closed {
		t.Fatal("stats not marked closed")
	}
}

func TestPoolParallelLeases(t *testing.T) {
	p, err := NewPool(compiledFactory(t), Options{MaxLive: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	const workers, per = 8, 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				l, err := p.Acquire(context.Background())
				if err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				// Each lease starts clean, so bump(1) always yields 1.
				if out, err := l.Invoke("bump", 1); err != nil || out[0] != 1 {
					t.Errorf("bump on clean instance = %v,%v want 1", out, err)
				}
				l.Release()
			}
		}()
	}
	wg.Wait()

	st := p.Stats()
	if st.Leased != 0 {
		t.Fatalf("leases outstanding after test: %d", st.Leased)
	}
	if st.Live > 8 {
		t.Fatalf("live %d exceeds MaxLive 8", st.Live)
	}
	if int(st.Acquired) != workers*per {
		t.Fatalf("acquired = %d, want %d", st.Acquired, workers*per)
	}
}

func TestNewPoolValidation(t *testing.T) {
	if _, err := NewPool(nil, Options{}); !errors.Is(err, ErrNoFactory) {
		t.Fatalf("nil factory = %v, want ErrNoFactory", err)
	}
	if _, err := NewPool(compiledFactory(t), Options{MaxLive: 2, MinIdle: 5}); err == nil {
		t.Fatal("MinIdle > MaxLive should error")
	}
	// Zero MaxLive defaults to OptimalInstances().
	p, err := NewPool(compiledFactory(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.Stats().Max != OptimalInstances() {
		t.Fatalf("default Max = %d, want %d", p.Stats().Max, OptimalInstances())
	}
}
