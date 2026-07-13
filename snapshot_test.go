package lease

import (
	"testing"

	"github.com/wago-org/wago"
)

// counterSnapshot compiles the counter module with explicit bounds (Capture
// rejects signals-based/guard-page modules) and captures a snapshot of it.
func counterSnapshot(t *testing.T, opts wago.SnapshotOptions) *wago.Snapshot {
	t.Helper()
	cfg := wago.NewRuntimeConfig().WithBoundsChecks(wago.BoundsChecksExplicit)
	c, err := wago.CompileWithConfig(cfg, counterModule())
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	snap, err := wago.Capture(c, opts)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	return snap
}

func TestFromSnapshotInitState(t *testing.T) {
	snap := counterSnapshot(t, wago.SnapshotOptions{Kind: wago.SnapshotInit})
	p, err := NewPool(FromSnapshot(snap), Options{MaxLive: 2, MinIdle: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	l := acquire(t, p)
	if got := get(t, l); got != 0 {
		t.Fatalf("init snapshot state = %d, want 0", got)
	}
	l.Release()
}

func TestFromSnapshotResetsToWarmState(t *testing.T) {
	// Warm snapshot: bump(100) runs before capture, so every restored instance
	// starts at 100 — proving the pool restores the captured (warm) state, and
	// that reset returns to it rather than to the module's declared zero state.
	snap := counterSnapshot(t, wago.SnapshotOptions{
		Kind: wago.SnapshotWarm, WarmFunc: "bump", WarmArgs: []uint64{100},
	})
	p, err := NewPool(FromSnapshot(snap), Options{MaxLive: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	l := acquire(t, p)
	if got := get(t, l); got != 100 {
		t.Fatalf("warm snapshot state = %d, want 100", got)
	}
	if got := bump(t, l, 5); got != 105 {
		t.Fatalf("bump = %d, want 105", got)
	}
	l.Release() // reset: discard the dirty instance, restore a fresh one at 100

	l = acquire(t, p)
	if got := get(t, l); got != 100 {
		t.Fatalf("state after reset = %d, want 100 (warm snapshot, not 105 or 0)", got)
	}
	l.Release()
}

func TestFromSnapshotDiskRoundTrip(t *testing.T) {
	snap := counterSnapshot(t, wago.SnapshotOptions{Kind: wago.SnapshotWarm, WarmFunc: "bump", WarmArgs: []uint64{7}})
	blob, err := snap.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !wago.IsSnapshot(blob) {
		t.Fatal("IsSnapshot(blob) = false")
	}
	loaded, err := wago.LoadSnapshot(blob)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p, err := NewPool(FromSnapshot(loaded), Options{MaxLive: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	l := acquire(t, p)
	if got := get(t, l); got != 7 {
		t.Fatalf("round-tripped snapshot state = %d, want 7", got)
	}
	l.Release()
}
