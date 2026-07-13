package lease

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func scale(full int) int {
	if testing.Short() {
		return full / 10
	}
	return full
}

// TestStressContendedAcquire drives far more goroutines than MaxLive so most
// acquisitions go through the blocking wait path, verifying instances are handed
// out one lease at a time, every lease starts clean, and accounting stays sane.
func TestStressContendedAcquire(t *testing.T) {
	p, err := NewPool(compiledFactory(t), Options{MaxLive: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	goroutines := 32
	per := scale(300)
	var wg sync.WaitGroup
	var done int64
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				l, err := p.Acquire(context.Background())
				if err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				// Clean state on every lease: bump(1) == 1, and no other lease can
				// touch this instance concurrently, so bump(1) again == 2.
				if out, err := l.Invoke("bump", 1); err != nil || out[0] != 1 {
					t.Errorf("bump#1 = %v,%v want 1", out, err)
				}
				if out, err := l.Invoke("bump", 1); err != nil || out[0] != 2 {
					t.Errorf("bump#2 = %v,%v want 2 (exclusive access)", out, err)
				}
				l.Release()
				atomic.AddInt64(&done, 1)
			}
		}()
	}
	wg.Wait()

	if int(done) != goroutines*per {
		t.Fatalf("completed %d, want %d", done, goroutines*per)
	}
	st := p.Stats()
	if st.Leased != 0 {
		t.Fatalf("%d leases still outstanding", st.Leased)
	}
	if st.Live > 4 {
		t.Fatalf("live %d exceeds MaxLive 4", st.Live)
	}
	if st.Waited == 0 {
		t.Fatal("expected contention (Waited > 0)")
	}
}

// TestStressCloseWhileBusy closes the pool while many goroutines are still
// acquiring/releasing, asserting a clean, deadlock-free shutdown.
func TestStressCloseWhileBusy(t *testing.T) {
	p, err := NewPool(compiledFactory(t), Options{MaxLive: 6, MinIdle: 3, AcquireTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	for g := 0; g < 12; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				l, err := p.Acquire(context.Background())
				if err != nil {
					if errors.Is(err, ErrPoolClosed) || errors.Is(err, ErrAcquireTimeout) {
						return
					}
					t.Errorf("acquire: %v", err)
					return
				}
				_, _ = l.Invoke("bump", 1)
				l.Release()
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)

	closed := make(chan struct{})
	go func() { _ = p.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung while busy")
	}
	stop.Store(true)
	wg.Wait()
	if !p.Stats().Closed {
		t.Fatal("pool not marked closed")
	}
}

// TestStressReuseParallel hammers the reuse policy across parallel leases: state
// accumulates on whichever instance a lease lands on, but exclusive access means
// each lease's own reads are consistent.
func TestStressReuseParallel(t *testing.T) {
	p, err := NewPool(compiledFactory(t), Options{MaxLive: 4, Release: Reuse})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < scale(200); i++ {
				l, err := p.Acquire(context.Background())
				if err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				got, err := l.Invoke("get")
				if err != nil {
					t.Errorf("get: %v", err)
					l.Release()
					return
				}
				before := got[0] // copy: Invoke's buffer is reused by the next call
				after, err := l.Invoke("bump", 1)
				if err != nil || after[0] != before+1 {
					t.Errorf("bump not exclusive: before=%d after=%v err=%v", before, after, err)
				}
				l.Release()
			}
		}()
	}
	wg.Wait()
	if st := p.Stats(); st.Leased != 0 || st.Live > 4 {
		t.Fatalf("post-stress stats = %+v", st)
	}
}
