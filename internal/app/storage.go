package app

import (
	"strconv"

	"github.com/Daniel-Vaz/KaaS-demo/internal/addons"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// The platform's default cluster storage. Every worker is born with one extra disk mounted at
// domain.LonghornDataPath, and the bundled longhorn add-on turns those disks into the cluster's
// default StorageClass - so a plain PVC with no storageClassName gets a real, replicated volume with
// nothing to configure.
//
// The whole feature deliberately adds NO storage concept of its own: Longhorn wants a mounted
// directory rather than a raw device, which is precisely what domain.NodeDisk already delivers. What
// lives here is only the part the catalog cannot express - a replica count that depends on the
// cluster's own worker count (see longhornAddonExtras).

// longhornAddon is the add-on this file configures, and the one whose presence decides whether a
// cluster gets platform storage disks at all (see storageDiskGB).
const longhornAddon = "longhorn"

// syncStorageDisks returns the cluster's disk list with the platform's per-worker storage disks
// materialized: one for every desired worker that does not already have one. Existing rows are
// returned untouched, which is the point - a disk carries OBSERVED state (its phase, the DeviceID
// the infrastructure reported), and re-minting it every admission would throw that away and re-run
// the format/mount step against a live filesystem.
//
// Callers MUST hold store.LockAdmission and MUST price the result: these disks are real capacity.
// Pair it with disksOnDesiredNodes, which prunes the disks of nodes a scale-down is taking away -
// this function only ever adds.
func syncStorageDisks(c *domain.Cluster) []domain.NodeDisk {
	have := make(map[string]bool, len(c.NodeDisks))
	for _, d := range c.NodeDisks {
		have[d.VMName+"/"+d.Name] = true
	}
	out := append([]domain.NodeDisk(nil), c.NodeDisks...)
	for _, d := range domain.DesiredStorageDisks(c) {
		if !have[d.VMName+"/"+d.Name] {
			out = append(out, d)
		}
	}
	return out
}

// hasInstalledAddon reports whether an add-on is installed on the cluster right now - not merely
// selected. The distinction matters for anything that talks to the add-on's own workloads (the
// storage UI tunnel): a chart still installing has no Service to proxy to.
func hasInstalledAddon(c *domain.Cluster, name string) bool {
	for _, a := range c.Addons {
		if a.Name == name && a.Phase == "installed" {
			return true
		}
	}
	return false
}

// longhornAddonExtras sets the cluster's Longhorn replica factor from its own worker count
// (domain.LonghornReplicas). Both keys must move together: defaultReplicaCount is the global default
// for volumes created through the API, and defaultClassReplicaCount is baked into the StorageClass
// the chart creates - leaving one at the chart's 3 would quietly give a two-worker cluster
// permanently degraded volumes.
//
// This rides in Extras rather than in catalog.json for a mechanical reason: a values override skips
// the catalog's `--set` values entirely (see helm.helmArgs), so a token substituted there would
// vanish for exactly the clusters whose values a user has edited. Extras are always applied, and
// applied last.
//
// Which is also why it stands down completely when the user HAS overridden the add-on's values. For
// external-dns, always winning is the point (a user must not be able to unhook their cluster from
// the platform's DNS). Here there is nothing to protect: the replica count is a default chosen on
// the user's behalf, and a user who opened the values editor and set one means it.
func longhornAddonExtras() addons.ExtrasFunc {
	return func(c *domain.Cluster, a domain.Addon) addons.Extras {
		if a.Name != longhornAddon || a.ValuesOverride != "" {
			return addons.Extras{}
		}
		replicas := strconv.Itoa(domain.LonghornReplicas(c))
		return addons.Extras{Values: map[string]string{
			"defaultSettings.defaultReplicaCount":  replicas,
			"persistence.defaultClassReplicaCount": replicas,
		}}
	}
}

// chainExtras composes per-add-on Extras providers into the single ExtrasFunc the helm manager
// takes. Each add-on is configured by exactly one of them (they key on a.Name), so the first
// non-empty answer wins and there is no merge to reason about.
func chainExtras(fns ...addons.ExtrasFunc) addons.ExtrasFunc {
	return func(c *domain.Cluster, a domain.Addon) addons.Extras {
		for _, fn := range fns {
			if e := fn(c, a); len(e.Values) > 0 || e.Secret != nil {
				return e
			}
		}
		return addons.Extras{}
	}
}

// StorageUIEnabled reports whether this cluster can serve the Storage page's "Open UI" link - i.e.
// the longhorn add-on is actually installed. The page asks so it can render the reason rather than
// a link that would 409.
func (a *App) StorageUIEnabled(c *domain.Cluster) bool { return hasInstalledAddon(c, longhornAddon) }
