// Package register exposes Lease's explicit provider catalog to generated Wago
// runtimes. Importing this package has no registration side effects.
package register

import (
	"github.com/JairusSW/lease"
	"github.com/wago-org/wago"
)

func Providers() []wago.PluginProvider {
	return []wago.PluginProvider{lease.Provider()}
}
