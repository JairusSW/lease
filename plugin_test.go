package lease

import (
	"context"
	"errors"
	"testing"

	"github.com/wago-org/wago"
)

func TestExtensionProvidesPoolAndClosesOnStop(t *testing.T) {
	snap := counterSnapshot(t, wago.SnapshotOptions{Kind: wago.SnapshotInit})
	pl := New(WithSnapshot(snap), WithOptions(Options{MaxLive: 3, MinIdle: 2}))

	rt := wago.NewRuntime()
	if err := rt.Use(pl); err != nil {
		t.Fatalf("use: %v", err)
	}
	pool := pl.Service()
	if pool == nil {
		t.Fatal("Service() is nil after Use")
	}
	l, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got := get(t, l); got != 0 {
		t.Fatalf("snapshot state = %d, want 0", got)
	}
	l.Release()

	// Closing the runtime stops the plugin, which closes the pool.
	if err := rt.Close(); err != nil {
		t.Fatalf("runtime close: %v", err)
	}
	if !pool.Stats().Closed {
		t.Fatal("pool was not closed on runtime Close")
	}
}

func TestExtensionRequiresSource(t *testing.T) {
	rt := wago.NewRuntime()
	defer rt.Close()
	if err := rt.Use(New()); !errors.Is(err, ErrNoFactory) {
		t.Fatalf("Use without a source = %v, want ErrNoFactory", err)
	}
}

func TestExtensionWithFactory(t *testing.T) {
	pl := New(WithFactory(compiledFactory(t)), WithOptions(Options{MaxLive: 2}))
	rt := wago.NewRuntime()
	defer rt.Close()
	if err := rt.Use(pl); err != nil {
		t.Fatalf("use: %v", err)
	}
	l, err := pl.Service().Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := bump(t, l, 9); got != 9 {
		t.Fatalf("bump = %d, want 9", got)
	}
	l.Release()
}
