<div align="center">
  <h1><code>lease</code></h1>
  <p>Bounded pools of clean WebAssembly instances for Wago.</p>
</div>

<p align="center">
  <a href="https://github.com/JairusSW/lease/actions/workflows/ci.yml"><img src="https://github.com/JairusSW/lease/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/go-%3E%3D1.24-00ADD8.svg" alt="Go >= 1.24"></a>
</p>

Lease keeps a bounded set of WebAssembly instances ready for one-shot work. A
caller acquires an instance, invokes it, then releases it. By default, release
closes that instance and mints a clean replacement, so state does not carry into
the next lease.

Use it as a Go library, or install it as a Wago plugin and let other plugins
contribute instance sources through a typed contract.

> Lease is experimental (`v0.1.0`). Its API may change before the first stable
> release.

## Install

As a Wago plugin:

```sh
wago add github.com/JairusSW/lease
```

As a Go library:

```sh
go get github.com/JairusSW/lease
```

## Use the pool directly

```go
compiled, err := wago.Compile(nil, moduleBytes)
if err != nil {
	log.Fatal(err)
}

pool, err := lease.NewPool(lease.FromCompiled(compiled), lease.Options{
	MaxLive: 8,
	MinIdle: 2,
})
if err != nil {
	log.Fatal(err)
}
defer pool.Close()

l, err := pool.Acquire(context.Background())
if err != nil {
	log.Fatal(err)
}
defer l.Release()

results, err := l.Invoke("handle", 42)
```

`FromCompiled` mints a fresh instance from a compiled module. `FromModule`
mints through a Wago runtime when the module needs that runtime's reviewed host
imports. You can also provide your own concurrency-safe `Factory`.

## Options

| Field | Default | Meaning |
| --- | --- | --- |
| `MaxLive` | `GOMAXPROCS` | Maximum live instances. Callers wait at capacity. |
| `MinIdle` | `0` | Clean instances created before the pool is returned. |
| `Release` | `Reset` | `Reset` replaces used instances; `Reuse` returns the same instance. |
| `AcquireTimeout` | none | Maximum wait at capacity, in addition to context cancellation. |

`Reuse` is only safe when calls leave no mutable state behind. `Reset` is a
close-and-remint operation; WebAssembly has no in-place instance reset.

`ErrPoolClosed`, `ErrAcquireTimeout`, `ErrLeaseReleased`, and `ErrNoFactory` are
sentinel errors suitable for `errors.Is`.

## Compose plugins

Lease's provider consumes every binding of
`github.com/JairusSW/lease/source@1` and provides
`github.com/JairusSW/lease/service@1`. The Wago lockfile records the exact
providers and their order, so adding a matching plugin cannot silently enter a
running dependency graph.

A source plugin supplies a fresh, caller-owned instance:

```go
var Source = &mySource{}

func Definition() wago.PluginDefinition {
	return wago.PluginDefinition{
		// ...ID, version, and provenance...
		Provides: []wago.ContractSpec{lease.SourceContract.Spec()},
	}
}

func (p *plugin) Register(reg *wago.Registrar) error {
	return wagoplugin.Provide(reg, lease.SourceContract, lease.Source(Source))
}

func (*mySource) ID() string { return "github.com/acme/handler" }

func (*mySource) Mint() (*wago.Instance, error) {
	return wago.Instantiate(compiled)
}
```

A project installs each desired source plugin explicitly. Lease consumes all
reviewed `source@1` bindings; it cannot discover or install unknown source
packages by contract name alone.

A consumer declares both the Lease package dependency and its required contract,
then requires the reviewed binding during registration:

```go
func Definition() wago.PluginDefinition {
	return wago.PluginDefinition{
		// ...ID, version, and provenance...
		Requires: []wago.PluginRequirement{{
			ID: lease.PluginID, Version: "^0.1.0",
		}},
		Consumes: []wago.ContractRequirement{{
			ID: lease.Contract.ID(), Major: lease.Contract.Major(),
			Mode: wago.ContractRequired,
		}},
	}
}
```

The package edge selects Lease transitively. The contract edge records exactly
which provider the consumer may call.

```go
service, err := wagoplugin.Require(reg, lease.Contract)
if err != nil {
	return err
}

err = service.With(func(s lease.Service) error {
	return s.WithLease(ctx, "github.com/acme/handler", func(l *lease.Lease) error {
		_, err := l.Invoke("handle", 42)
		return err
	})
})
```

Both callbacks are part of the ownership boundary. Do not retain the `Service`,
`Lease`, or instance after its callback returns. Wago rejects new calls when a
consumer stops, waits for its in-flight calls, and stops providers only after
their consumers. Lease can therefore close its pools without racing active
cross-plugin calls.

The plugin configuration is strict and rejects unknown fields:

```sh
wago plugin config github.com/JairusSW/lease \
  '{"maxLive":8,"minIdle":2,"release":"reset","acquireTimeoutMillis":5000,"maxSources":64}'
```

## Test

```sh
go test ./...
go test -race ./...
```

The suite covers capacity, reset and reuse, cancellation, shutdown, and a full
source → Lease → consumer contract graph.

## License

Apache-2.0. See [LICENSE](./LICENSE).
