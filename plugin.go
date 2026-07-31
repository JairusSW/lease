package lease

import (
	"context"

	"github.com/wago-org/wago"
	"github.com/wago-org/wago/plugin"
)

// PluginName is the registration name of the lease plugin.
const PluginName = "JairusSW/lease"

// ServiceKey exposes the pool to composing plugins.
var ServiceKey = plugin.NewServiceKey[*Pool]("wago.lease/v1")

// Plugin is a thin wago.Extension over a Pool: register it on a runtime to have
// the pool closed on runtime shutdown and shared with other plugins through
// ServiceKey. It is configured programmatically (a snapshot or factory is a
// runtime object, not manifest data), so it is not globally registered for
// manifest loading — construct it with New and pass it to Runtime.Use.
type Plugin struct {
	factory Factory
	opts    Options
	pool    *Pool
}

// Option configures the Plugin.
type Option func(*Plugin)

// WithSnapshot pools instances restored from snap (via FromSnapshot).
func WithSnapshot(snap *wago.Snapshot) Option {
	return func(p *Plugin) { p.factory = FromSnapshot(snap) }
}

// WithCompiled pools instances minted from a compiled module (via FromCompiled) —
// a non-snapshot source. Pass Imports as opts for modules with host imports.
func WithCompiled(c *wago.Compiled, opts ...any) Option {
	return func(p *Plugin) { p.factory = FromCompiled(c, opts...) }
}

// WithModule pools instances minted from a runtime-bound module (via FromModule) —
// a non-snapshot source whose instances get the runtime's host imports.
func WithModule(rt *wago.Runtime, mod *wago.Module) Option {
	return func(p *Plugin) { p.factory = FromModule(rt, mod) }
}

// WithFactory pools instances minted by an arbitrary factory.
func WithFactory(factory Factory) Option { return func(p *Plugin) { p.factory = factory } }

// WithOptions sets the pool Options.
func WithOptions(o Options) Option { return func(p *Plugin) { p.opts = o } }

// New creates the lease plugin. Provide a source with WithSnapshot or WithFactory.
func New(opts ...Option) *Plugin {
	p := &Plugin{}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (*Plugin) Info() wago.ExtensionInfo {
	return wago.ExtensionInfo{
		ID: "wago.lease", Name: "Lease", Version: "0.0.0",
		Description: "A pool of stateless WebAssembly instances (snapshot-, compiled-, or module-backed) leased for one-shot execution",
		Stability:   wago.Experimental, Repository: "https://github.com/JairusSW/lease",
		License: "Apache-2.0",
		Tags:    []string{"lease", "pool", "snapshot", "stateless", "faas", "sandbox"},
		Compat:  wago.Compatibility{Engines: map[string]string{"wago": ">=0.1.0"}},
		// The lease plugin uses no core capabilities: snapshot instances are
		// unmanaged and minted by the factory.
	}
}

func (p *Plugin) Register(reg *wago.Registry) error {
	if p.factory == nil {
		return ErrNoFactory
	}
	pool, err := NewPool(p.factory, p.opts)
	if err != nil {
		return err
	}
	p.pool = pool
	return plugin.Provide(reg, ServiceKey, pool)
}

// Stop closes the pool, releasing every idle instance.
func (p *Plugin) Stop(context.Context) error {
	if p == nil || p.pool == nil {
		return nil
	}
	return p.pool.Close()
}

// Service returns the Pool, or nil before Register.
func (p *Plugin) Service() *Pool {
	if p == nil {
		return nil
	}
	return p.pool
}
