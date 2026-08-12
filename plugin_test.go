package lease

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"

	"github.com/wago-org/wago"
	wagoplugin "github.com/wago-org/wago/plugin"
)

type registerFunc func(*wago.Registrar) error

func (f registerFunc) Register(reg *wago.Registrar) error { return f(reg) }

type testSource struct {
	id      string
	factory Factory
}

func (s *testSource) ID() string                    { return s.id }
func (s *testSource) Mint() (*wago.Instance, error) { return s.factory() }

func pluginTestDefinition(id string) wago.PluginDefinition {
	return wago.PluginDefinition{
		ID: id, Version: "1.0.0",
		Provenance: wago.PluginProvenance{Repository: "https://" + id, License: "MIT"},
	}
}

func sourceProvider(id, sourceID string, factory Factory) wago.PluginProvider {
	definition := pluginTestDefinition(id)
	definition.Provides = []wago.ContractSpec{SourceContract.Spec()}
	return wago.PluginProvider{Definition: definition, New: func() wago.Plugin {
		return registerFunc(func(reg *wago.Registrar) error {
			return wagoplugin.Provide(reg, SourceContract, Source(&testSource{id: sourceID, factory: factory}))
		})
	}}
}

func consumerProvider(ref **wagoplugin.Ref[Service]) wago.PluginProvider {
	definition := pluginTestDefinition("example.com/lease-consumer")
	definition.Requires = []wago.PluginRequirement{{ID: PluginID, Version: "^0.1.0"}}
	definition.Consumes = []wago.ContractRequirement{{ID: Contract.ID(), Major: Contract.Major(), Mode: wago.ContractRequired}}
	return wago.PluginProvider{Definition: definition, New: func() wago.Plugin {
		return registerFunc(func(reg *wago.Registrar) error {
			var err error
			*ref, err = wagoplugin.Require(reg, Contract)
			return err
		})
	}}
}

func pluginTestSet(t *testing.T, providers []wago.PluginProvider, config json.RawMessage) wago.PluginSet {
	t.Helper()
	set := wago.PluginSet{Providers: providers}
	for _, provider := range providers {
		digest, err := wago.DefinitionDigest(provider.Definition)
		if err != nil {
			t.Fatal(err)
		}
		selection := wago.PluginSelection{
			ID: provider.Definition.ID, DefinitionDigest: digest, Direct: true,
			Dependencies: map[string]string{},
		}
		for _, requirement := range provider.Definition.Requires {
			selection.Dependencies[requirement.ID] = requirement.Version
		}
		if provider.Definition.ID == PluginID {
			selection.Config = append(json.RawMessage(nil), config...)
		}
		for _, requirement := range provider.Definition.Consumes {
			var owners []string
			for _, candidate := range providers {
				for _, provided := range candidate.Definition.Provides {
					if provided.ID == requirement.ID && provided.Major == requirement.Major {
						owners = append(owners, candidate.Definition.ID)
					}
				}
			}
			sort.Strings(owners)
			if requirement.Mode != wago.ContractMany && len(owners) > 1 {
				owners = owners[:1]
			}
			selection.Contracts = append(selection.Contracts, wago.ContractBinding{ID: requirement.ID, Major: requirement.Major, Providers: owners})
		}
		set.Selections = append(set.Selections, selection)
	}
	return set
}

func TestPluginComposesSourceThroughLeasedContract(t *testing.T) {
	var serviceRef *wagoplugin.Ref[Service]
	source := sourceProvider("example.com/lease-source", "counter", compiledFactory(t))
	// Reverse the graph so ContractMany orders the source before Lease and the
	// package plus contract edges order Lease before its consumer.
	set := pluginTestSet(t, []wago.PluginProvider{consumerProvider(&serviceRef), Provider(), source}, json.RawMessage(`{"maxLive":2,"minIdle":1}`))
	rt := wago.NewRuntime()
	if err := rt.LoadPlugins(context.Background(), set); err != nil {
		t.Fatal(err)
	}

	if err := serviceRef.With(func(service Service) error {
		return service.WithLease(context.Background(), "counter", func(lease *Lease) error {
			if got := bump(t, lease, 9); got != 9 {
				t.Fatalf("bump = %d, want 9", got)
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := serviceRef.With(func(service Service) error {
		return service.WithLease(context.Background(), "counter", func(lease *Lease) error {
			if got := get(t, lease); got != 0 {
				t.Fatalf("reset state = %d, want 0", got)
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}

	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := serviceRef.With(func(Service) error { return nil }); !errors.Is(err, wago.ErrPermissionDenied) {
		t.Fatalf("contract after close = %v", err)
	}
}

func TestPluginAllowsNoSourcesAndFailsUnknownClosed(t *testing.T) {
	var serviceRef *wagoplugin.Ref[Service]
	set := pluginTestSet(t, []wago.PluginProvider{Provider(), consumerProvider(&serviceRef)}, nil)
	rt := wago.NewRuntime()
	if err := rt.LoadPlugins(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	err := serviceRef.With(func(service Service) error {
		return service.WithLease(context.Background(), "missing", func(*Lease) error { return nil })
	})
	if !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("missing source = %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPluginRejectsDuplicateSourcesAndStrictConfig(t *testing.T) {
	a := sourceProvider("example.com/lease-source-a", "same", compiledFactory(t))
	b := sourceProvider("example.com/lease-source-b", "same", compiledFactory(t))
	if err := wago.NewRuntime().LoadPlugins(context.Background(), pluginTestSet(t, []wago.PluginProvider{a, b, Provider()}, nil)); !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("duplicate source = %v", err)
	}
	set := pluginTestSet(t, []wago.PluginProvider{Provider()}, json.RawMessage(`{"unknown":1}`))
	if err := wago.ValidatePluginSet(set); err == nil {
		t.Fatal("accepted unknown config field")
	}
	set = pluginTestSet(t, []wago.PluginProvider{Provider()}, json.RawMessage(`{"maxLive":1,"minIdle":2}`))
	if err := wago.ValidatePluginSet(set); err == nil {
		t.Fatal("accepted minIdle above maxLive")
	}
	for _, config := range []json.RawMessage{
		json.RawMessage(`null`),
		json.RawMessage(`[]`),
		json.RawMessage(`{"maxLive":null}`),
		json.RawMessage(`{"maxLive":1,"maxLive":2}`),
		json.RawMessage(`{"maxLive":0}`),
		json.RawMessage(`{"maxSources":0}`),
		json.RawMessage(`{"release":""}`),
		json.RawMessage(`{} {}`),
	} {
		set = pluginTestSet(t, []wago.PluginProvider{Provider()}, config)
		if err := wago.ValidatePluginSet(set); err == nil {
			t.Fatalf("accepted invalid config %s", config)
		}
	}
}
