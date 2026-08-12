package lease

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wago-org/wago"
	wagoplugin "github.com/wago-org/wago/plugin"
)

const PluginID = "github.com/JairusSW/lease"

var (
	// SourceContract lets execution-model plugins contribute instance factories
	// without giving Lease raw Runtime authority.
	SourceContract = wagoplugin.NewContract[Source](PluginID+"/source", 1)
	// Contract exposes callback-scoped one-shot execution to other plugins.
	Contract = wagoplugin.NewContract[Service](PluginID+"/service", 1)

	ErrSourceNotFound  = errors.New("lease: source not found")
	ErrDuplicateSource = errors.New("lease: duplicate source")
	ErrSourceLimit     = errors.New("lease: source limit exceeded")
)

// Source is implemented by a plugin that can transfer ownership of a fresh,
// clean instance to Lease. Mint runs while the provider's Contract lease is
// held; implementations must not return a shared or already-borrowed instance.
type Source interface {
	ID() string
	Mint() (*wago.Instance, error)
}

// Service is Lease's typed cross-plugin API. WithLease prevents the borrowed
// instance and the Service implementation from escaping their Contract lease.
type Service interface {
	WithLease(context.Context, string, func(*Lease) error) error
	Stats(string) (Stats, bool)
}

type pluginConfig struct {
	MaxLive              *int    `json:"maxLive,omitempty"`
	MinIdle              *int    `json:"minIdle,omitempty"`
	Release              *string `json:"release,omitempty"`
	AcquireTimeoutMillis *int64  `json:"acquireTimeoutMillis,omitempty"`
	MaxSources           *int    `json:"maxSources,omitempty"`
}

var configSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "maxLive": {"type": "integer", "minimum": 1, "maximum": 1024},
    "minIdle": {"type": "integer", "minimum": 0, "maximum": 1024},
    "release": {"type": "string", "enum": ["reset", "reuse"]},
    "acquireTimeoutMillis": {"type": "integer", "minimum": 0, "maximum": 86400000},
    "maxSources": {"type": "integer", "minimum": 1, "maximum": 1024}
  }
}`)

func decodePluginConfig(raw json.RawMessage) (pluginConfig, Options, int, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := validateConfigObject(raw); err != nil {
		return pluginConfig{}, Options{}, 0, fmt.Errorf("lease: config: %w", err)
	}
	var cfg pluginConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return pluginConfig{}, Options{}, 0, fmt.Errorf("lease: config: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return pluginConfig{}, Options{}, 0, fmt.Errorf("lease: config has a trailing JSON value")
	}
	maxLive, minIdle, acquireTimeoutMillis, maxSources := 0, 0, int64(0), 64
	if cfg.MaxLive != nil {
		maxLive = *cfg.MaxLive
	}
	if cfg.MinIdle != nil {
		minIdle = *cfg.MinIdle
	}
	if cfg.AcquireTimeoutMillis != nil {
		acquireTimeoutMillis = *cfg.AcquireTimeoutMillis
	}
	if cfg.MaxSources != nil {
		maxSources = *cfg.MaxSources
	}
	if cfg.MaxLive != nil && (maxLive < 1 || maxLive > 1024) || minIdle < 0 || minIdle > 1024 ||
		cfg.MaxLive != nil && minIdle > maxLive || acquireTimeoutMillis < 0 ||
		acquireTimeoutMillis > 86_400_000 || cfg.MaxSources != nil && (maxSources < 1 || maxSources > 1024) {
		return pluginConfig{}, Options{}, 0, fmt.Errorf("lease: config limits are out of range")
	}
	policy := Reset
	release := "reset"
	if cfg.Release != nil {
		release = *cfg.Release
	}
	switch release {
	case "reset":
	case "reuse":
		policy = Reuse
	default:
		return pluginConfig{}, Options{}, 0, fmt.Errorf("lease: unsupported release policy %q", release)
	}
	return cfg, Options{
		MaxLive: maxLive, MinIdle: minIdle, Release: policy,
		AcquireTimeout: time.Duration(acquireTimeoutMillis) * time.Millisecond,
	}, maxSources, nil
}

func validateConfigObject(raw json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return fmt.Errorf("must be a JSON object")
	}
	seen := map[string]struct{}{}
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("object key is not a string")
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("duplicate field %q", key)
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("field %q must not be null", key)
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		if err == nil {
			return fmt.Errorf("has a trailing JSON value")
		}
		return err
	}
	return nil
}

// Definition returns fresh immutable metadata for Lease's explicit provider.
func Definition() wago.PluginDefinition {
	return wago.PluginDefinition{
		ID:          PluginID,
		Name:        "Lease",
		Version:     "0.1.0",
		Description: "Bounded pools of clean WebAssembly instances for one-shot execution.",
		Stability:   wago.Experimental,
		Compatibility: wago.Compatibility{
			Engines: map[string]string{"wago": ">=0.1.0"},
		},
		Provenance: wago.PluginProvenance{
			Homepage:   "https://github.com/JairusSW/lease",
			Repository: "https://github.com/JairusSW/lease",
			License:    "Apache-2.0",
			Authors:    []string{"Jairus Tanaka"},
		},
		ConfigSchema: append(json.RawMessage(nil), configSchema...),
		Provides:     []wago.ContractSpec{Contract.Spec()},
		Consumes: []wago.ContractRequirement{{
			ID: SourceContract.ID(), Major: SourceContract.Major(), Mode: wago.ContractMany,
		}},
	}
}

// Provider is Lease's side-effect-free catalog entry.
func Provider() wago.PluginProvider {
	return wago.PluginProvider{
		Definition: Definition(),
		New:        func() wago.Plugin { return new(plugin) },
		ValidateConfig: func(raw json.RawMessage) error {
			_, _, _, err := decodePluginConfig(raw)
			return err
		},
	}
}

type plugin struct {
	sources *wagoplugin.ManyRef[Source]
	service *service
}

func (p *plugin) Register(reg *wago.Registrar) error {
	var cfg pluginConfig
	if err := reg.Config(&cfg); err != nil {
		return err
	}
	opts, maxSources := optionsFromPluginConfig(cfg)
	var err error
	p.sources, err = wagoplugin.Many(reg, SourceContract)
	if err != nil {
		return err
	}
	p.service = &service{sources: p.sources, options: opts, maxSources: maxSources, pools: map[string]*Pool{}}
	if err := wagoplugin.Provide(reg, Contract, Service(p.service)); err != nil {
		return err
	}
	return reg.Lifecycle(wago.PluginLifecycle{Start: p.service.start, Stop: p.service.stop})
}

func optionsFromPluginConfig(cfg pluginConfig) (Options, int) {
	opts := Options{}
	maxSources := 64
	if cfg.MaxLive != nil {
		opts.MaxLive = *cfg.MaxLive
	}
	if cfg.MinIdle != nil {
		opts.MinIdle = *cfg.MinIdle
	}
	if cfg.Release != nil && *cfg.Release == "reuse" {
		opts.Release = Reuse
	}
	if cfg.AcquireTimeoutMillis != nil {
		opts.AcquireTimeout = time.Duration(*cfg.AcquireTimeoutMillis) * time.Millisecond
	}
	if cfg.MaxSources != nil {
		maxSources = *cfg.MaxSources
	}
	return opts, maxSources
}

type service struct {
	sources    *wagoplugin.ManyRef[Source]
	options    Options
	maxSources int

	mu     sync.Mutex
	pools  map[string]*Pool
	closed bool
}

func (s *service) start(context.Context) error {
	ids, err := s.sourceIDs()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.pool(id); err != nil {
			_ = s.stop(context.Background())
			return fmt.Errorf("lease: start source %q: %w", id, err)
		}
	}
	return nil
}

func (s *service) WithLease(ctx context.Context, sourceID string, fn func(*Lease) error) error {
	if fn == nil {
		return fmt.Errorf("lease: nil lease callback")
	}
	pool, err := s.pool(sourceID)
	if err != nil {
		return err
	}
	lease, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer lease.Release()
	return fn(lease)
}

func (s *service) Stats(sourceID string) (Stats, bool) {
	s.mu.Lock()
	pool := s.pools[sourceID]
	s.mu.Unlock()
	if pool == nil {
		return Stats{}, false
	}
	return pool.Stats(), true
}

func (s *service) sourceIDs() ([]string, error) {
	var ids []string
	err := s.sources.With(func(sources []Source) error {
		seen := make(map[string]struct{}, len(sources))
		for _, source := range sources {
			if source == nil {
				return fmt.Errorf("lease: nil source")
			}
			id := source.ID()
			if len(id) == 0 || len(id) > 300 || strings.TrimSpace(id) != id {
				return fmt.Errorf("lease: invalid source ID %q", id)
			}
			if _, duplicate := seen[id]; duplicate {
				return fmt.Errorf("%w %q", ErrDuplicateSource, id)
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(ids) > s.maxSources {
		return nil, fmt.Errorf("%w: got %d, max %d", ErrSourceLimit, len(ids), s.maxSources)
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *service) pool(sourceID string) (*Pool, error) {
	if sourceID == "" {
		return nil, fmt.Errorf("%w: empty source ID", ErrSourceNotFound)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrPoolClosed
	}
	if pool := s.pools[sourceID]; pool != nil {
		return pool, nil
	}
	if len(s.pools) >= s.maxSources {
		return nil, fmt.Errorf("%w: max %d", ErrSourceLimit, s.maxSources)
	}
	factory := func() (*wago.Instance, error) { return s.mint(sourceID) }
	pool, err := NewPool(factory, s.options)
	if err != nil {
		return nil, err
	}
	s.pools[sourceID] = pool
	return pool, nil
}

func (s *service) mint(sourceID string) (*wago.Instance, error) {
	var instance *wago.Instance
	found := 0
	err := s.sources.With(func(sources []Source) error {
		for _, source := range sources {
			if source.ID() != sourceID {
				continue
			}
			found++
			if found > 1 {
				return fmt.Errorf("%w %q", ErrDuplicateSource, sourceID)
			}
			var err error
			instance, err = source.Mint()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if found == 0 {
		return nil, fmt.Errorf("%w %q", ErrSourceNotFound, sourceID)
	}
	if instance == nil {
		return nil, fmt.Errorf("lease: source %q returned a nil instance", sourceID)
	}
	return instance, nil
}

func (s *service) stop(context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ids := make([]string, 0, len(s.pools))
	for id := range s.pools {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	pools := make([]*Pool, 0, len(ids))
	for _, id := range ids {
		pools = append(pools, s.pools[id])
	}
	s.mu.Unlock()
	var errs []error
	for i := len(pools) - 1; i >= 0; i-- {
		errs = append(errs, pools[i].Close())
	}
	return errors.Join(errs...)
}
