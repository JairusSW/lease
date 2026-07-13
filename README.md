<div align="center">
    <h1><code>lease</code></h1>
    <p>A pool of stateless WebAssembly instances for the <a href="https://github.com/wago-org/wago">Wago</a> runtime — acquire a clean instance, call it, reset, release.</p>
</div>

<p align="center">
    <a href="https://github.com/JairusSW/lease/actions/workflows/ci.yml"><img src="https://github.com/JairusSW/lease/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
    <a href="https://go.dev/"><img src="https://img.shields.io/badge/go-%3E%3D1.24-00ADD8.svg" alt="Go >= 1.24"></a>
    <a href="https://github.com/wago-org/wago"><img src="https://img.shields.io/badge/wago-%3E%3D0.1.0-6E56CF.svg" alt="Wago >= 0.1.0"></a>
</p>

## Overview

`lease` pools **stateless WebAssembly instances** for one-shot execution — the serverless / FaaS model. You keep a set of warm, clean instances; a caller **acquires** one, **invokes** one or more exported functions, then **releases** it. On release the instance is reset to clean state and its capacity returns to the pool. Instances run in **parallel** — one lease at a time per instance, so a pool of N instances serves N concurrent calls.

The clean instances come from an instance **source**. The warm-start source is a [wago **snapshot**](https://github.com/wago-org/wago) (`FromSnapshot`): `Capture` warms a module once (running its init / start), and every restored instance starts from that captured image **without re-running init**. You can also pool non-snapshot instances — a plain compiled module (`FromCompiled`) or a runtime-bound module (`FromModule`, whose instances get the runtime's **host imports** — WASI, other plugins — which snapshot instances don't). Those cold-start each mint but pool and reset identically. Because WebAssembly has no in-place "reset instance" operation, a **reset is a discard + re-mint**: the used instance is closed and a fresh one minted from the source. Every mint reproduces the same clean state, so every lease begins from exactly it.

What you get:

- **Isolation per call** — each lease is a fresh instance; no state leaks between requests or tenants.
- **Warm starts** — restore skips module init; `MinIdle` keeps instances pre-warmed.
- **Bounded parallelism** — `MaxLive` caps concurrent instances (default: the machine's CPU parallelism, so you fill the cores without oversubscribing them).
- **Backpressure** — acquiring past capacity blocks until a lease returns, with an optional timeout or context cancellation.
- **Reset or reuse** — reset to clean state on every release (default), or reuse instances for truly stateless handlers.

> **Stability:** experimental (`v0.1.0`). The API may change before `v1.0.0`.

### Use cases

- **Per-request handler execution** — run a request handler on a clean instance, reset, return it. Warm-start latency + isolation.
- **Multi-tenant / untrusted code** — execute per-tenant or user-submitted Wasm with guaranteed fresh state each call.
- **Sandboxed evaluation** — policy engines, template rendering, expression/formula eval — each evaluation isolated and reproducible.
- **Parallel stateless transforms** — apply the same `input → output` function across many inputs concurrently.

## Installation

```sh
go get github.com/JairusSW/lease
```

## Concepts

| Term | Meaning |
| --- | --- |
| **Pool** (`*lease.Pool`) | A bounded, self-warming set of instances leased one at a time. |
| **Lease** (`*lease.Lease`) | A checked-out instance. Invoke exports on it, then Release it. |
| **Factory** | `func() (*wago.Instance, error)` — mints a fresh clean instance. Usually `FromSnapshot`. |
| **Reset** | On release: close the used instance and restore a clean replacement from the snapshot. |

## Usage

```go
package main

import (
	"context"
	"log"

	"github.com/wago-org/wago"
	"github.com/JairusSW/lease"
)

func main() {
	// 1. Compile and warm-capture a snapshot of your handler module. Snapshots are
	//    explicit-bounds only, so compile with BoundsChecksExplicit.
	cfg := wago.NewRuntimeConfig().WithBoundsChecks(wago.BoundsChecksExplicit)
	compiled, err := wago.CompileWithConfig(cfg, handlerWasm)
	if err != nil {
		log.Fatal(err)
	}
	snap, err := wago.Capture(compiled, wago.SnapshotOptions{
		Kind: wago.SnapshotWarm, WarmFunc: "_start", // run init once, capture the warm state
	})
	if err != nil {
		log.Fatal(err)
	}

	// 2. Pool it. MaxLive 0 sizes to the CPU; MinIdle pre-warms.
	pool, err := lease.NewPool(lease.FromSnapshot(snap), lease.Options{MinIdle: 4})
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	// 3. Per request: acquire a clean instance, call it, release it.
	l, err := pool.Acquire(context.Background())
	if err != nil {
		log.Fatal(err) // ErrAcquireTimeout / context error when at capacity
	}
	out, err := l.Invoke("handle", 42)
	l.Release() // reset to clean state, return capacity to the pool
	log.Printf("handle -> %v (%v)", out, err)
}
```

### If your module needs host imports

Restored instances get **only the imports captured with the snapshot** — not a runtime's plugin imports. For a pure-compute module (no host imports), the pool is fully self-contained. If your module imports host functions, supply them in `SnapshotOptions.Imports` at `Capture` time and keep the snapshot **in memory** (a snapshot written to disk drops import closures). The captured imports are re-applied to every restored instance automatically.

## API

```go
func NewPool(factory Factory, opts Options) (*Pool, error)

// Instance sources (pick one, or supply your own Factory):
func FromSnapshot(snap *wago.Snapshot) Factory                 // warm restore, skips init
func FromCompiled(c *wago.Compiled, opts ...any) Factory       // plain module, cold start
func FromModule(rt *wago.Runtime, mod *wago.Module) Factory    // runtime-wired: gets host imports

func OptimalInstances() int

func (p *Pool) Acquire(ctx context.Context) (*Lease, error)
func (p *Pool) Stats() Stats
func (p *Pool) Close() error

func (l *Lease) Invoke(export string, args ...uint64) ([]uint64, error)
func (l *Lease) Instance() *wago.Instance // escape hatch, e.g. Instance.Call(ctx, ...)
func (l *Lease) Release()
```

`Invoke`'s returned slots are backed by an instance-owned buffer reused on the next
call — copy them if you keep them past the next `Invoke`.

### Options

| Field | Default | Meaning |
| --- | --- | --- |
| `MaxLive` | `OptimalInstances()` (GOMAXPROCS) | Cap on total instances (idle + leased). Acquiring past it blocks. |
| `MinIdle` | `0` | Instances pre-warmed at construction. Must be ≤ `MaxLive`. |
| `Release` | `Reset` | `Reset` (discard + restore clean) or `Reuse` (return the same instance unreset). |
| `AcquireTimeout` | `0` (wait) | Max time `Acquire` waits at capacity before `ErrAcquireTimeout`. |

`OptimalInstances()` returns `GOMAXPROCS` — the recommended `MaxLive` for CPU-bound
work, so the pool fills the cores without oversubscribing them. Raise `MaxLive`
explicitly for I/O-bound handlers that spend time waiting.

### Errors

`ErrPoolClosed`, `ErrAcquireTimeout`, `ErrLeaseReleased`, `ErrNoFactory` — comparable sentinels; match with `errors.Is`.

### As a Wago plugin

For composition on a runtime, wrap the pool in the `wago.Extension`: it provides the
`*Pool` via `ServiceKey` and closes it on runtime shutdown.

```go
pl := lease.New(lease.WithSnapshot(snap), lease.WithOptions(lease.Options{MinIdle: 4}))
rt := wago.NewRuntime()
rt.Use(pl)
pool := pl.Service()
// ... rt.Close() stops the plugin and closes the pool.
```

It is configured programmatically (a snapshot is a runtime object, not manifest data), so it is not registered for manifest loading.

## Design notes

- **Reset = discard + restore.** There is no in-place instance reset in the engine; restoring from a snapshot skips init and reproduces the captured image, which is the natural, cheap reset. The cost is an `O(memory)` copy per reset, paid on release.
- **Self-sizing warmth.** The pool grows lazily to peak concurrency (up to `MaxLive`) and keeps a clean instance ready in each slot after a reset, so acquisitions stay warm without a background maintainer goroutine.
- **Snapshots are explicit-bounds only.** `Capture` rejects signals-based (guard-page) modules, modules with tables, and reference globals. Compile with `WithBoundsChecks(BoundsChecksExplicit)`.

## Testing

```sh
go test ./...
go test -race ./...   # the pool is concurrent; the suite runs under -race
go test -short ./...  # smaller stress sizes
```

The suite covers the pool mechanics (acquire/release/reset/reuse, blocking,
timeout, context cancellation, limits, parallel leases, close-while-busy) against a
plainly compiled counter module, plus a snapshot integration path proving reset
returns instances to the captured warm state, and a disk round-trip.

## License

Distributed under the [Apache License 2.0](./LICENSE).
