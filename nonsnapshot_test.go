package lease

import (
	"context"
	"testing"

	"github.com/wago-org/wago"
	"github.com/wago-org/wago/testutil/wasmtest"
)

func TestFromCompiledResets(t *testing.T) {
	c, err := wago.Compile(nil, counterModule())
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPool(FromCompiled(c), Options{MaxLive: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	l := acquire(t, p)
	bump(t, l, 9)
	l.Release() // reset: fresh compiled instance next time

	l = acquire(t, p)
	if got := get(t, l); got != 0 {
		t.Fatalf("state after reset = %d, want 0 (fresh compiled instance)", got)
	}
	l.Release()
}

// addBaseModule imports env.base() -> i32 and exports addbase(n) -> base()+n,
// so a working instance requires the host import to be wired.
func addBaseModule() []byte {
	typeBase := []byte{0x60, 0x00, 0x01, 0x7f}      // () -> (i32)
	typeAdd := []byte{0x60, 0x01, 0x7f, 0x01, 0x7f} // (i32) -> (i32)
	types := wasmtest.Vec(typeBase, typeAdd)

	imp := append(wasmtest.Name("env"), wasmtest.Name("base")...)
	imp = append(imp, 0x00)                // func import
	imp = append(imp, wasmtest.ULEB(0)...) // type 0
	imports := wasmtest.Vec(imp)

	funcs := wasmtest.Vec(wasmtest.ULEB(1)) // local func0=addbase, type 1 (import is func0 overall)
	exports := wasmtest.Vec(wasmtest.ExportEntry("addbase", 0x00, 1))
	// addbase: call base (func 0); local.get 0; i32.add.
	code := wasmtest.Vec(wasmtest.Code([]byte{0x10, 0x00, 0x20, 0x00, 0x6a, 0x0b}))

	return wasmtest.Module(
		wasmtest.Section(1, types),
		wasmtest.Section(2, imports),
		wasmtest.Section(3, funcs),
		wasmtest.Section(7, exports),
		wasmtest.Section(10, code),
	)
}

// baseHost is a minimal extension exporting env.base() -> a constant.
type baseHost struct{ val uint64 }

func (h *baseHost) Info() wago.ExtensionInfo {
	return wago.ExtensionInfo{
		ID: "lease.test.base", RequiresCapabilities: []wago.PluginCapability{wago.PluginHostImports},
	}
}

func (h *baseHost) Register(reg *wago.Registry) error {
	imports, err := reg.HostImports()
	if err != nil {
		return err
	}
	imports.Module("env").Func("base", func(_ wago.HostModule, _, results []uint64) {
		results[0] = h.val
	}).Results(wago.ValI32)
	return nil
}

// TestFromModuleWiresHostImports pools a module that calls a host import and
// verifies the runtime wiring reaches every pooled instance — the capability that
// snapshot instances (which carry only captured imports) do not have.
func TestFromModuleWiresHostImports(t *testing.T) {
	rt := wago.NewRuntime()
	defer rt.Close()
	if err := rt.Use(&baseHost{val: 100}); err != nil {
		t.Fatalf("use host: %v", err)
	}
	mod, err := rt.Compile(addBaseModule())
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	p, err := NewPool(FromModule(rt, mod), Options{MaxLive: 3, MinIdle: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for i := 0; i < 5; i++ {
		l, err := p.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		out, err := l.Invoke("addbase", 5)
		if err != nil || out[0] != 105 {
			t.Fatalf("addbase(5) = %v,%v want 105 (host base 100 + 5)", out, err)
		}
		l.Release()
	}
}

func TestFromConstructorsNilGuards(t *testing.T) {
	if _, err := FromSnapshot(nil)(); err == nil {
		t.Fatal("FromSnapshot(nil) mint should error")
	}
	if _, err := FromCompiled(nil)(); err == nil {
		t.Fatal("FromCompiled(nil) mint should error")
	}
	if _, err := FromModule(nil, nil)(); err == nil {
		t.Fatal("FromModule(nil,nil) mint should error")
	}
}
