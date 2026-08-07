// Package reconcile is the level-triggered control loop. It diffs desired vs.
// observed state and advances each cluster through its lifecycle state machine, calling
// the provision/config/addons seams. Every step is idempotent and emits events.
//
// In the real system these phase transitions are River jobs; here the loop drives them
// directly so the pattern is legible in one file.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/addons"
	"github.com/Daniel-Vaz/KaaS-demo/internal/catalog"
	"github.com/Daniel-Vaz/KaaS-demo/internal/config"
	"github.com/Daniel-Vaz/KaaS-demo/internal/dns"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/events"
	"github.com/Daniel-Vaz/KaaS-demo/internal/health"
	"github.com/Daniel-Vaz/KaaS-demo/internal/metrics"
	"github.com/Daniel-Vaz/KaaS-demo/internal/monitoring"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision"
	"github.com/Daniel-Vaz/KaaS-demo/internal/registry"
	"github.com/Daniel-Vaz/KaaS-demo/internal/secrets"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
	"github.com/Daniel-Vaz/KaaS-demo/internal/vault"
)

type Reconciler struct {
	Store store.Store
	// Prov is the default provisioner (the deployment's first enabled infrastructure provider).
	// Provs holds one per provider name (domain.ProviderKVM/…); a cluster is always provisioned,
	// destroyed and garbage-collected through the one its Provider field names - see prov(). In
	// fake mode every name maps to the same *provision.Fake.
	Prov   provision.Provisioner
	Provs  map[string]provision.Provisioner
	Cfg    config.Manager
	Addons addons.Manager
	// DNS publishes each cluster's platform-owned apps wildcard in the site's DNS and withdraws it
	// before the infrastructure is destroyed (see internal/dns). Nil is "this deployment publishes
	// no DNS" - every call site is guarded, so tests that build a Reconciler by hand need not set it.
	DNS dns.Registrar
	// Vault provisions each cluster's Vault path + policies + the External-Secrets auth role
	// (reconcileVaultWiring / releaseVault) and converges the per-user/-group access bindings under
	// the leader lease (SyncAccess). Nil is "this deployment runs no Vault" - every call site is
	// guarded, so hand-built test reconcilers need not set it. See internal/vault.
	Vault vault.Manager
	// Registry provisions each cluster's image-registry project + push/pull robot
	// (reconcileRegistryWiring / releaseRegistry) and converges the per-user project memberships
	// under the leader lease (SyncAccess). Nil is "this deployment runs no registry" - every call
	// site is guarded, so hand-built test reconcilers need not set it. See internal/registry.
	Registry registry.Manager
	Metrics  metrics.Collector // live resource-usage telemetry (read-only seam)
	Health   health.Checker    // live cluster-health checks (read-only seam)
	Catalog  *catalog.Catalog  // resolves bundles + diffs them for the upgrade dispatch
	Secrets  *secrets.Box
	Events   events.Sink
	Log      *slog.Logger
	Interval time.Duration
	// CertRenewWindow enables automatic control-plane certificate rotation: when a Ready cluster's
	// certificates fall within this of expiry, the reconciler renews them (PhaseRenewingCerts). Zero
	// disables the feature entirely - no cert observation, no renewal. Set from KAAS_CERT_RENEW /
	// KAAS_CERT_RENEW_WINDOW (see cmd/worker).
	CertRenewWindow time.Duration
	// EtcdPolicy governs automatic etcd maintenance: how often a Ready cluster's backend store is
	// observed, and when its fragmentation is worth the stop-the-world cost of defragmenting
	// (PhaseDefragmentingEtcd). The zero value - Enabled false - disables the feature entirely, no
	// observation included. Set from KAAS_ETCD_* / KAAS_MAINTENANCE_WINDOW (see internal/app).
	EtcdPolicy domain.EtcdDefragPolicy
	// SnapshotPolicy governs periodic control-plane backups: how often a Ready cluster's etcd is
	// snapshotted (PhaseSnapshottingEtcd), how many are kept, and how stale one may be and still be
	// restored. The zero value - Enabled false - disables the feature, which also makes a dead SOLE
	// control plane unrecoverable, since a restore can only ever consume a stored snapshot. Set from
	// KAAS_ETCD_SNAPSHOT_* (see internal/app).
	SnapshotPolicy domain.EtcdSnapshotPolicy
	// RepairPolicy governs automatic cluster and node repair: when a fault is believed, which rung of
	// the escalation ladder applies, and - mostly - when the platform must refuse to act at all. The
	// zero value disables the feature, observation included. Set from KAAS_REPAIR_* (see internal/app).
	RepairPolicy domain.RepairPolicy
}

// Run ticks the reconcile loop until ctx is cancelled. Level-triggered: it re-reads
// desired state every tick, so it self-heals and is safe to restart. A slower ticker runs
// the orphan GC sweep independently of per-cluster reconciliation.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	gc := time.NewTicker(gcInterval)
	defer gc.Stop()
	mt := time.NewTicker(metricsInterval)
	defer mt.Stop()
	ht := time.NewTicker(healthInterval)
	defer ht.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.reconcileAll(ctx)
		case <-gc.C:
			r.GC(ctx)
		case <-mt.C:
			r.CollectMetrics(ctx)
		case <-ht.C:
			r.CheckHealth(ctx)
		}
	}
}

// gcInterval is how often the orphan sweep runs (both the tick loop and the River mode).
const gcInterval = 30 * time.Second

// metricsInterval is how often live resource-usage is sampled from Ready clusters. Slower than
// the reconcile tick - this is telemetry for a dashboard, not a control signal.
const metricsInterval = 15 * time.Second

// healthInterval is how often each Ready cluster's health checks are evaluated. Like metrics this
// is observed telemetry, not a control signal, so it runs on its own slow ticker.
const healthInterval = 20 * time.Second

// vaultSyncInterval is how often the leader converges Vault's per-user/-group access bindings with
// the store. A membership edit is an API-side write invisible to the per-cluster loop, so this sweep
// is what eventually reflects it in Vault - a slow ticker is fine (access changes are rare, and the
// portal's own authorization is already correct the instant the edit lands).
const vaultSyncInterval = 30 * time.Second

// registrySyncInterval is the same idea for the image registry's project memberships, and slower:
// each sweep reads every project's member list, which is a round-trip per project rather than a
// single write, and losing pull access a minute later than losing portal access is not a meaningful
// exposure - the portal is already the authoritative gate on everything a user reaches through it.
const registrySyncInterval = 60 * time.Second

// CollectMetrics samples live per-node resource usage from every Ready cluster whose
// metrics-server add-on is installed, and upserts the latest snapshot into the store. It is
// read-only telemetry decoupled from the reconcile state machine (a Ready cluster isn't in
// ClustersNeedingWork, so it never reaches reconcileOne): a per-cluster failure is logged and
// retried next sweep, never fatal. No collector configured → a no-op.
func (r *Reconciler) CollectMetrics(ctx context.Context) {
	if r.Metrics == nil {
		return
	}
	clusters, err := r.Store.ListClusters()
	if err != nil {
		r.Log.Error("metrics: list clusters", "err", err)
		return
	}
	for _, c := range clusters {
		if c.Phase != domain.PhaseReady || !metricsEnabled(c) {
			continue
		}
		if err := r.collectOne(ctx, c); err != nil {
			// Telemetry is best-effort - a not-yet-ready metrics-server or a blip shouldn't spam
			// the event timeline. Log at debug and move on; the next sweep retries.
			r.Log.Debug("metrics: collect", "cluster", c.Name, "err", err)
		}
	}
}

func (r *Reconciler) collectOne(ctx context.Context, c *domain.Cluster) error {
	kubeconfig, err := r.getSecret(c.ID, domain.SecretKubeconfig)
	if err != nil {
		return fmt.Errorf("kubeconfig unavailable: %w", err)
	}
	nodes, err := r.Metrics.CollectNodes(ctx, c, kubeconfig)
	if err != nil {
		return err
	}
	return r.Store.SaveMetrics(&domain.MetricsSnapshot{
		ClusterID:   c.ID,
		CollectedAt: time.Now(),
		Nodes:       nodes,
	})
}

// CheckHealth evaluates the dedicated health checks against every Ready cluster and upserts the
// latest snapshot into the store. Like CollectMetrics it is read-only telemetry decoupled from the
// reconcile state machine - it never changes a cluster's Phase - and a per-cluster failure is
// logged and retried next sweep, never fatal. Gated only on Ready (a health check needs just the
// API server + kubeconfig, unlike metrics which also needs metrics-server). No checker → a no-op.
func (r *Reconciler) CheckHealth(ctx context.Context) {
	if r.Health == nil {
		return
	}
	clusters, err := r.Store.ListClusters()
	if err != nil {
		r.Log.Error("health: list clusters", "err", err)
		return
	}
	for _, c := range clusters {
		if c.Phase != domain.PhaseReady {
			continue
		}
		if err := r.checkHealthOne(ctx, c); err != nil {
			// Best-effort telemetry - a transient blip shouldn't spam the event timeline.
			r.Log.Debug("health: check", "cluster", c.Name, "err", err)
		}
	}
}

func (r *Reconciler) checkHealthOne(ctx context.Context, c *domain.Cluster) error {
	kubeconfig, err := r.getSecret(c.ID, domain.SecretKubeconfig)
	if err != nil {
		return fmt.Errorf("kubeconfig unavailable: %w", err)
	}
	res, err := r.Health.Check(ctx, c, kubeconfig)
	if err != nil {
		return err
	}
	// Two checks are appended here rather than produced by the Checker: both are derived purely from
	// stored control-plane state, and both need a POLICY the checker has no business knowing about
	// (how often backups should happen, whether repair is enabled). Threading those through the seam
	// would make every Checker implementation depend on configuration it never reads. Same trick
	// CertCheck plays with observed state, one level up.
	checks := append(res.Checks,
		health.BackupCheck(c, r.SnapshotPolicy),
		health.RepairCheck(c, r.RepairPolicy),
	)
	return r.Store.SaveHealth(&domain.HealthSnapshot{
		ClusterID:   c.ID,
		Status:      domain.RollupHealth(checks),
		Checks:      checks,
		Nodes:       res.Nodes,
		CollectedAt: time.Now(),
	})
}

// metricsEnabled reports whether the cluster's metrics-server add-on is installed, so it can
// serve usage. A cluster with it removed (or never selected) exposes no metrics.
func metricsEnabled(c *domain.Cluster) bool {
	for _, a := range c.Addons {
		if a.Name == metrics.AddonName && a.Phase == "installed" {
			return true
		}
	}
	return false
}

// monitoringInstalled reports whether the kube-prometheus-stack add-on is installed - i.e. Prometheus
// (and its Operator + ServiceMonitor CRD) are present, which is the precondition for
// reconcileMonitoringWiring.
func monitoringInstalled(c *domain.Cluster) bool {
	for _, a := range c.Addons {
		if a.Name == monitoring.AddonName && a.Phase == "installed" {
			return true
		}
	}
	return false
}

// reconcileMonitoringWiring makes the cluster's control plane and CNI actually scrapeable once the
// monitoring stack is installed, but not before (the CNI's ServiceMonitor needs the Prometheus
// Operator's CRD, which doesn't exist until the stack installs; the control-plane fix has no such
// CRD dependency, but is gated the same way so a cluster that never asked for monitoring doesn't have
// its control-plane metrics ports opened for nothing). Called right after reconcileAddons, so on a
// fresh bring-up the stack installs and everything else is wired in the same tick.
//
// Both steps are idempotent (a helm upgrade; manifest edits + a ConfigMap merge patch) but not free,
// so we skip them once the cluster's MonitoringWired marker is set and only re-run when something can
// actually undo the wiring has cleared it (a CNI helm (re)install, or a control-plane node rolled onto
// a fresh golden image - see finalizeHop and rollOneNode). Without the marker this fired on every
// unrelated update tick (e.g. an add-on values edit), re-running the ansible control-plane role for
// nothing.
func (r *Reconciler) reconcileMonitoringWiring(ctx context.Context, c *domain.Cluster) error {
	if !monitoringInstalled(c) || c.MonitoringWired {
		return nil
	}
	if err := r.Cfg.EnsureControlPlaneMetrics(ctx, c); err != nil {
		return err
	}
	r.emit(c.ID, "info", "ansible", "wired the control plane (etcd, scheduler, controller-manager, kube-proxy) for Prometheus scraping")
	if err := r.Cfg.EnsureCNIMetrics(ctx, c); err != nil {
		return err
	}
	r.emit(c.ID, "info", "ansible", fmt.Sprintf("wired %q metrics into the monitoring stack (ServiceMonitors)", c.CNI))
	c.MonitoringWired = true
	return nil
}

// The default-gateway add-ons. Both ship in the bundle, so a stock cluster gets a LoadBalancer
// implementation (metallb) and a Gateway API (envoy-gateway) out of the box; a user may deselect
// them, in which case the wiring below never fires.
const (
	metallbAddon      = "metallb"
	envoyGatewayAddon = "envoy-gateway"
	// certManagerAddon terminates HTTPS on the default Gateway. It ships in the bundle too, but is
	// optional: when selected the gateway wiring additionally applies a self-signed ClusterIssuer and
	// (when the cluster has an apps domain) a wildcard certificate + an HTTPS listener. Because that
	// wiring latches on GatewayWired, it must not run until cert-manager's CRDs exist - see
	// gatewayWiringReady.
	certManagerAddon = "cert-manager"
	// longhornAddon backs the cluster's default StorageClass with the per-worker storage disks the
	// platform provisions. Bundled like the two above, and deselectable in the same way - a cluster
	// without it keeps its disks as plain mounted filesystems and skips reconcileStorageWiring.
	longhornAddon = "longhorn"
)

// addonInstalled reports whether a named add-on is installed on the cluster right now (as opposed to
// selected, still installing, or on its way out).
func addonInstalled(c *domain.Cluster, name string) bool {
	for _, a := range c.Addons {
		if a.Name == name && a.Phase == "installed" {
			return true
		}
	}
	return false
}

// gatewayWiringReady reports whether the cluster is ready for its default MetalLB pool + Envoy
// Gateway to be applied: both add-ons installed AND an address reserved for the pool. A cluster
// whose metallb/envoy-gateway add-ons were deselected, or one predating the reserved-IP feature,
// never becomes ready and the wiring is simply skipped.
func gatewayWiringReady(c *domain.Cluster) bool {
	if c.LoadBalancerIP == "" {
		return false
	}
	var metallb, envoy bool
	// cert-manager is optional but, when selected, contributes to the SAME one-shot wiring pass (the
	// issuer + wildcard cert + HTTPS listener). GatewayWired latches after that pass, so if we wired
	// before cert-manager finished installing its CRDs, the TLS half would be skipped forever. Hence:
	// a selected-but-not-yet-installed cert-manager holds the whole wiring back.
	certManagerSelected, certManagerInstalled := false, false
	for _, a := range c.Addons {
		if a.Name == certManagerAddon && a.Phase != "removing" {
			certManagerSelected = true
		}
		if a.Phase != "installed" {
			continue
		}
		switch a.Name {
		case metallbAddon:
			metallb = true
		case envoyGatewayAddon:
			envoy = true
		case certManagerAddon:
			certManagerInstalled = true
		}
	}
	if certManagerSelected && !certManagerInstalled {
		return false
	}
	return metallb && envoy
}

// reconcileGatewayWiring applies the cluster's default north-south ingress once metallb + envoy-gateway
// are installed and an address is reserved: a MetalLB IPAddressPool/L2Advertisement built from
// c.LoadBalancerIP, plus the Envoy GatewayClass + a default Gateway pinned to it (see the
// default_gateway ansible role). Called right after reconcileAddons, so on a fresh bring-up the
// add-ons install and the gateway is wired in the same run to Ready.
//
// Idempotent (`kubectl apply`) but not free, so the GatewayWired marker skips it once done. Unlike
// MonitoringWired nothing clears the marker: the CRs live in etcd, so a CNI/OS upgrade or a node roll
// does not undo them.
func (r *Reconciler) reconcileGatewayWiring(ctx context.Context, c *domain.Cluster) error {
	if !gatewayWiringReady(c) || c.GatewayWired {
		return nil
	}
	if err := r.Cfg.EnsureDefaultGateway(ctx, c); err != nil {
		return err
	}
	r.emit(c.ID, "info", "ansible", fmt.Sprintf("configured the default MetalLB pool and Envoy Gateway on %s", c.LoadBalancerIP))
	c.GatewayWired = true
	return nil
}

// reconcileStorageWiring registers a node's EXTRA storage disks with the in-cluster Longhorn, so
// they count as pool capacity (see the longhorn_disks role). Called right after reconcileAddons,
// alongside the monitoring and gateway wiring it is modelled on.
//
// It differs from those two in the one way that matters: its marker is a FINGERPRINT of the disk set
// rather than a bool. MonitoringWired and GatewayWired guard work that is decided once - the
// cluster's reserved address does not move - but a user attaches and removes storage disks on a
// running cluster, and each change has to reach the node.longhorn.io CRs. A bool would latch on the
// first disk and silently strand every one after it.
//
// The common cluster does no work here at all: a worker's platform disk is mounted at Longhorn's own
// default data path, so longhorn-manager registers it unprompted and the fingerprint stays empty.
func (r *Reconciler) reconcileStorageWiring(ctx context.Context, c *domain.Cluster) error {
	want := domain.StorageFingerprint(c)
	// No longhorn on this cluster means no CRs to patch - and the disks are still perfectly good
	// mounted filesystems, so this is a skip, not an error.
	if want == c.StorageWired || !addonInstalled(c, longhornAddon) {
		return nil
	}
	if err := r.Cfg.EnsureLonghornDisks(ctx, c); err != nil {
		return err
	}
	r.emit(c.ID, "info", "ansible", "registered extra node disks with Longhorn")
	c.StorageWired = want
	return nil
}

// reconcileDNSWiring publishes the cluster's platform-owned apps wildcard -
// "*.apps.<cluster>.kaas.<domain>. A <LoadBalancerIP>" - once the gateway that address fronts is
// actually wired. Publishing earlier would resolve users at an address nothing answers on yet; the
// order costs nothing, since both run in the same pass to Ready.
//
// The upsert is idempotent, so the DNSWired marker is only an optimisation (a Kerberos handshake
// plus an update packet per tick is not free). Nothing clears it: the record lives in the site's DNS, which no
// cluster operation touches. A failure fails the step and the level-triggered loop retries - same
// rule as the NetBox registration, and for the same reason: silently skipping would leave the zone
// permanently out of step with reality, which on a recycled address means resolving a user into
// someone else's cluster.
func (r *Reconciler) reconcileDNSWiring(ctx context.Context, c *domain.Cluster) error {
	if r.DNS == nil || c.AppsDomain == "" || !c.GatewayWired || c.DNSWired {
		return nil
	}
	if err := r.DNS.EnsureCluster(ctx, c); err != nil {
		return err
	}
	r.emit(c.ID, "info", "dns", fmt.Sprintf("published %s → %s", dns.Wildcard(c.AppsDomain), c.LoadBalancerIP))
	c.DNSWired = true
	return nil
}

// releaseDNS withdraws the cluster's wildcard. It runs BEFORE the infrastructure is destroyed, so
// the name stops resolving while the gateway it points at is still the cluster's own: the reserved
// address goes back to the pool on destroy and the next cluster on that subnet may well get it, and
// a wildcard left behind would route this tenant's users straight into that one. Idempotent -
// deleting an absent record succeeds - so it is safe on every deleting tick and on a retry.
func (r *Reconciler) releaseDNS(ctx context.Context, c *domain.Cluster) error {
	if r.DNS == nil || c.AppsDomain == "" {
		return nil
	}
	if err := r.DNS.ReleaseCluster(ctx, c); err != nil {
		return err
	}
	if c.DNSWired {
		r.emit(c.ID, "info", "dns", fmt.Sprintf("withdrew %s", dns.Wildcard(c.AppsDomain)))
	}
	c.DNSWired = false
	return nil
}

// prov is the provisioner for a cluster's infrastructure provider. A cluster records the
// provider it was created on (immutable), so it is always provisioned, scaled, rolled and
// destroyed through the same backend. Unknown/legacy providers fall back to the default.
func (r *Reconciler) prov(c *domain.Cluster) provision.Provisioner {
	if p, ok := r.Provs[c.InfraProvider()]; ok {
		return p
	}
	return r.Prov
}

// provisioners is every distinct provisioner this reconciler drives, deduped by identity (in
// fake mode all provider names share one *provision.Fake, and sweeping it once per name would
// re-destroy the same orphans).
func (r *Reconciler) provisioners() []provision.Provisioner {
	out := make([]provision.Provisioner, 0, len(r.Provs)+1)
	seen := map[provision.Provisioner]bool{}
	for _, p := range r.Provs {
		if p != nil && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if r.Prov != nil && !seen[r.Prov] {
		out = append(out, r.Prov)
	}
	return out
}

// GC is the orphan sweep: it destroys any provisioned infrastructure whose cluster no
// longer exists (or is fully deleted) in the store - self-healing after a crash between
// "cluster deleted" and "VMs destroyed", or an out-of-band change. Clusters merely stuck
// in Deleting are handled by the normal loop (Deleting isn't terminal), not here.
//
// It sweeps every provisioner: infrastructure is orphaned per backend, and the cluster row
// that would tell us which backend built it is exactly what's gone - so each provisioner is
// asked what it still has, and destroys its own orphans.
func (r *Reconciler) GC(ctx context.Context) {
	var clusters []*domain.Cluster
	for _, p := range r.provisioners() {
		managed, err := p.ListManaged(ctx)
		if err != nil {
			r.Log.Error("gc: list managed infra", "err", err)
			continue
		}
		if len(managed) == 0 {
			continue
		}
		if clusters == nil { // read the live set once, and only if some provisioner has infra
			clusters, err = r.Store.ListClusters()
			if err != nil {
				r.Log.Error("gc: list clusters", "err", err)
				return
			}
		}
		live := make(map[string]bool, len(clusters))
		for _, c := range clusters {
			if c.Phase != domain.PhaseDeleted {
				live[c.ID] = true
			}
		}
		for _, id := range managed {
			if live[id] {
				continue
			}
			r.Log.Warn("gc: destroying orphaned infrastructure", "cluster", id)
			r.emit(id, "info", "gc", "orphaned infrastructure detected - destroying")
			if err := p.DestroyCluster(ctx, id); err != nil {
				r.Log.Error("gc: destroy orphan", "cluster", id, "err", err)
			}
		}
	}
}

// certRenewEnabled reports whether automatic control-plane certificate rotation is turned on.
func (r *Reconciler) certRenewEnabled() bool { return r.CertRenewWindow > 0 }

// certRenewCutoff is the expiry horizon below which certificates are renewed: now + the configured
// window. A cluster whose earliest cert expiry falls before this is due for rotation.
func (r *Reconciler) certRenewCutoff() time.Time { return time.Now().Add(r.CertRenewWindow) }

// clustersNeedingWork is the reconciler's work set: the generation-driven set (ClustersNeedingWork)
// unioned, when automatic certificate rotation is enabled, with the time-driven set of Ready clusters
// due for renewal (ClustersDueCertRenewal). The union is deduped by id, so a cluster that is both a
// generation edit and cert-due is reconciled once. Both the tick loop and the River enqueuer go
// through here, so the two modes surface the same work.
func (r *Reconciler) clustersNeedingWork() ([]*domain.Cluster, error) {
	clusters, err := r.Store.ClustersNeedingWork()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(clusters))
	for _, c := range clusters {
		seen[c.ID] = true
	}
	add := func(due []*domain.Cluster) {
		for _, c := range due {
			if !seen[c.ID] {
				seen[c.ID] = true
				clusters = append(clusters, c)
			}
		}
	}
	if r.certRenewEnabled() {
		due, err := r.Store.ClustersDueCertRenewal(r.certRenewCutoff())
		if err != nil {
			return nil, err
		}
		add(due)
	}
	if r.EtcdPolicy.Enabled {
		due, err := r.Store.ClustersDueEtcdMaintenance(time.Now().Add(-r.EtcdPolicy.ObserveInterval))
		if err != nil {
			return nil, err
		}
		add(due)
	}
	if r.SnapshotPolicy.Enabled {
		due, err := r.Store.ClustersDueEtcdSnapshot(time.Now().Add(-r.SnapshotPolicy.Interval))
		if err != nil {
			return nil, err
		}
		add(due)
	}
	if r.RepairPolicy.Enabled {
		due, err := r.Store.ClustersDueRepair(time.Now().Add(-r.RepairPolicy.ObserveInterval))
		if err != nil {
			return nil, err
		}
		add(due)
	}
	return clusters, nil
}

func (r *Reconciler) reconcileAll(ctx context.Context) {
	clusters, err := r.clustersNeedingWork()
	if err != nil {
		r.Log.Error("list clusters needing work", "err", err)
		return
	}
	for _, c := range clusters {
		if err := r.reconcileOne(ctx, c); err != nil {
			// A failure just leaves desired != observed; the next tick retries.
			r.Log.Error("reconcile", "cluster", c.Name, "phase", c.Phase, "err", err)
			r.emit(c.ID, "error", "reconciler", fmt.Sprintf("%s failed: %v - will retry", c.Phase, err))
		}
	}
}

// reconcileOne advances a single cluster by one phase per tick, so progress is visible.
func (r *Reconciler) reconcileOne(ctx context.Context, c *domain.Cluster) error {
	switch c.Phase {
	case domain.PhasePending:
		r.emit(c.ID, "info", "reconciler", "provisioning infrastructure")
		c.Phase = domain.PhaseProvisioningInfra

	case domain.PhaseProvisioningInfra:
		specs := r.nodeSpecs(c)
		nodes, err := r.prov(c).EnsureNodes(ctx, c.ID, netSpec(c), specs)
		if err != nil {
			return err
		}
		c.Nodes = toDomainNodes(specs, nodes)
		// Every cluster is now born with disks (the per-worker storage disk), so their identities have
		// to be picked up here and not only on the Updating path - nothing formats a disk whose
		// DeviceID the infrastructure has not reported.
		observeDisks(c, nodes)
		r.emit(c.ID, "info", "infra", fmt.Sprintf("provisioned %d VM(s)", len(c.Nodes)))
		c.Phase = domain.PhaseInfraReady

	case domain.PhaseInfraReady:
		kubeconfig, joinToken, err := r.Cfg.InitControlPlane(ctx, c)
		if err != nil {
			return err
		}
		if err := r.saveSecret(c.ID, domain.SecretKubeconfig, kubeconfig); err != nil {
			return err
		}
		if err := r.saveSecret(c.ID, domain.SecretJoinToken, []byte(joinToken)); err != nil {
			return err
		}
		if c.HA() {
			r.emit(c.ID, "info", "ansible", fmt.Sprintf("HA control plane initialized (%d nodes, VIP %s)", c.ControlPlaneCount(), c.APIVIP))
		} else {
			r.emit(c.ID, "info", "ansible", "control plane initialized (kubeadm init)")
		}
		c.Phase = domain.PhaseControlPlaneReady

	case domain.PhaseControlPlaneReady:
		if err := r.Cfg.JoinWorkers(ctx, c); err != nil {
			return err
		}
		r.emit(c.ID, "info", "ansible", fmt.Sprintf("%d worker(s) joined", c.WorkerCount()))
		c.Phase = domain.PhaseWorkersReady

	case domain.PhaseWorkersReady:
		if err := r.Cfg.InstallCNI(ctx, c); err != nil {
			return err
		}
		r.emit(c.ID, "info", "ansible", fmt.Sprintf("installed CNI %q", c.CNI))
		// Mint the read-only viewer kubeconfig (RBAC + a bound ServiceAccount token) once the API
		// server is up, so read-role group-mates get a genuinely read-only credential to download and
		// to open the shell with - never cluster-admin. Idempotent; the Ready phase re-ensures it for
		// clusters created before this step existed.
		if err := r.ensureViewerKubeconfig(ctx, c); err != nil {
			return err
		}
		// Format and mount the extra disks the cluster was CREATED with - the per-worker storage disk
		// every cluster gets (domain.DesiredStorageDisks). This has to happen here, before add-ons,
		// and not only on the Updating path where disks used to be the sole preserve of a Ready
		// cluster: Longhorn takes over /var/lib/longhorn the moment it installs, and if that path is
		// still the node's root disk at the time, the cluster's storage silently lands on the root
		// filesystem and the real disk is never used.
		if err := r.mountNodeDisks(ctx, c); err != nil {
			return err
		}
		r.emit(c.ID, "info", "reconciler", "installing add-ons")
		c.Phase = domain.PhaseInstallingAddons

	case domain.PhaseInstallingAddons:
		// Add-on installation is its own phase so its progress is visible on its own (the monitoring
		// stack alone can take minutes). The bundle's platform add-ons install first - kube-prometheus-stack
		// brings up the ServiceMonitor CRD that later add-ons depend on (ordering pinned in resolveAddons).
		if err := r.reconcileAddons(ctx, c); err != nil {
			return err
		}
		// Now that add-ons (incl. the monitoring stack, installed first) are up, wire the CNI's
		// ServiceMonitors - it couldn't publish them during bootstrap, before the CRD existed.
		if err := r.reconcileMonitoringWiring(ctx, c); err != nil {
			return err
		}
		// With metallb + envoy-gateway installed, apply the default MetalLB pool + Envoy Gateway.
		if err := r.reconcileGatewayWiring(ctx, c); err != nil {
			return err
		}
		// With the gateway answering on the reserved address, publish the cluster's apps wildcard.
		if err := r.reconcileDNSWiring(ctx, c); err != nil {
			return err
		}
		// With external-secrets installed, provision the cluster's Vault path + policies and point ESO
		// at Vault (see reconcileVaultWiring).
		if err := r.reconcileVaultWiring(ctx, c); err != nil {
			return err
		}
		// And the cluster's own image-registry project + pull credential (see reconcileRegistryWiring).
		// Unlike the Vault wiring this gates on no add-on: nothing has to be installed for a cluster to
		// have somewhere to push.
		if err := r.reconcileRegistryWiring(ctx, c); err != nil {
			return err
		}
		// And with Longhorn up, hand it any disk beyond each worker's platform one (usually none -
		// see reconcileStorageWiring).
		if err := r.reconcileStorageWiring(ctx, c); err != nil {
			return err
		}
		c.Phase = domain.PhaseReady
		// Only claim the generation is observed if the provisioned nodes actually match the
		// (possibly since-edited) desired set. If the user scaled during bring-up, leave
		// ObservedGeneration behind so the Ready→Updating path re-converges - the
		// level-triggered guarantee.
		if r.nodesConverged(c) {
			c.ObservedGeneration = c.Generation
			c.Status = "cluster ready"
			r.emit(c.ID, "info", "reconciler", "cluster ready")
		} else {
			r.emit(c.ID, "info", "reconciler", "desired state changed during bring-up - reconciling")
		}

	case domain.PhaseReady:
		// Self-heal: a cluster created before the read-only viewer kubeconfig existed won't have one.
		// Mint it opportunistically - only when absent, so this is a no-op on every normal Ready tick.
		if _, err := r.getSecret(c.ID, domain.SecretKubeconfigViewer); errors.Is(err, store.ErrNotFound) {
			if err := r.ensureViewerKubeconfig(ctx, c); err != nil {
				return err
			}
		}
		// A pending bundle promotion takes precedence over a plain scale/add-on edit (both bump
		// generation); the upgrade path finishes catching observed_generation up.
		if c.UpgradePending() {
			r.emit(c.ID, "info", "reconciler", fmt.Sprintf("upgrade requested - promoting toward %q", c.TargetBundle))
			c.Phase = domain.PhaseUpgrading
		} else if c.ObservedGeneration != c.Generation {
			r.emit(c.ID, "info", "reconciler", "change detected - updating")
			c.Phase = domain.PhaseUpdating
		} else if c.RepairDue(r.RepairPolicy, time.Now()) {
			// Ranked FIRST among the time-driven work, and below the two generation-driven paths.
			//
			// Above certificates, snapshots and defragmentation because it is the only one of the
			// four that is not maintenance: those three are deadlines and hygiene on a cluster that
			// is working, and this one runs when a cluster is not. Below upgrade and update because
			// those are the user's explicit intent - and, more practically, because both of them
			// drain, remove and rebuild nodes ON PURPOSE, and every node they touch looks exactly
			// like the faults this repairs. (RepairDue's Ready-and-converged guard already excludes
			// that case; the ranking means repair also never pre-empts the work that would have
			// fixed the cluster anyway, since a full converge pass re-runs EnsureNodes and JoinWorkers.)
			if err := r.reconcileRepair(ctx, c); err != nil {
				return err
			}
		} else if r.certRenewEnabled() && c.CertRenewalDue(r.certRenewCutoff()) {
			// Time-driven, not generation-driven: the only reason a Ready, converged cluster is in the
			// work set (once upgrade/update are ruled out) is that it's cert-due or etcd-due. Observe
			// expiry if we've never seen it, and renew only if it's actually within the window. Ranks
			// below a real upgrade/scale - which itself reissues certs - so those always win the tick.
			if err := r.reconcileCerts(ctx, c); err != nil {
				return err
			}
		} else if c.EtcdSnapshotDue(r.SnapshotPolicy, time.Now()) {
			// Above defragmentation, below certificate rotation. Above defrag deliberately: a defrag
			// on a sole control plane is a brief outage of the only API server there is, and the one
			// thing that makes that trade safer is having taken a backup first. With one phase per
			// invocation, ranking the snapshot higher means a cluster due for both snapshots this
			// tick and defragments the next - which is exactly the order wanted, for free. It also
			// closes the "production would snapshot etcd first on a sole control plane" gap the
			// defrag feature left open.
			r.emit(c.ID, "info", "reconciler", "control-plane backup due")
			c.Phase = domain.PhaseSnapshottingEtcd
		} else if c.EtcdMaintenanceDue(r.EtcdPolicy, time.Now()) {
			// Ranked LAST: everything above is either a deadline or an outage, and a defrag is
			// discretionary. Nothing is lost by deferring - the etcd observation is still due on the
			// next tick.
			if err := r.reconcileEtcd(ctx, c); err != nil {
				return err
			}
		}

	case domain.PhaseRenewingCerts:
		if err := r.reconcileCertRenewal(ctx, c); err != nil {
			return err
		}

	case domain.PhaseDefragmentingEtcd:
		if err := r.reconcileDefrag(ctx, c); err != nil {
			return err
		}

	case domain.PhaseSnapshottingEtcd:
		// Take an ONLINE etcd snapshot plus the cluster PKI and kubelet state, seal it, store it, and
		// prune the oldest beyond the retention count. Nothing on the cluster is stopped, so unlike
		// every other phase in this switch it costs the user nothing.
		if err := r.reconcileSnapshot(ctx, c); err != nil {
			return err
		}
		c.Phase = domain.PhaseReady

	case domain.PhaseRepairing:
		// Execute the one repair the Ready tick decided on and recorded. The attempt was already
		// stamped before we got here, so a failure below leaves the counter incremented and the
		// backoff running - which is the point: a repair that crashes mid-flight must still count.
		if err := r.executeRepair(ctx, c); err != nil {
			// Deliberately not returned. A failed repair is an ordinary outcome of repairing a broken
			// thing, not a reconcile error to be retried with River's backoff - the ladder has its own
			// backoff and its own give-up counter, and letting the job retry would run the same rung
			// again immediately, outside the policy that decided it was due.
			r.Log.Warn("repair failed", "cluster", c.Name, "target", c.RepairState().Target, "err", err)
			r.emit(c.ID, "error", "reconciler", fmt.Sprintf("repair of %s (%s) failed: %v",
				c.RepairState().Target, c.RepairState().Action, err))
		}
		c.RepairState().CompleteAttempt()
		c.Phase = domain.PhaseReady

	case domain.PhaseUpgrading:
		if err := r.reconcileUpgrade(ctx, c); err != nil {
			return err
		}

	case domain.PhaseUpdating:
		specs := r.nodeSpecs(c)
		// Scale-down: drain + delete workers that are no longer desired BEFORE their VMs
		// are destroyed, so the cluster doesn't keep a stale NotReady node.
		if removed := removedWorkers(c.Nodes, specs); len(removed) > 0 {
			r.emit(c.ID, "info", "ansible", fmt.Sprintf("removing %d worker(s)", len(removed)))
			if err := r.Cfg.RemoveWorkers(ctx, c, removed); err != nil {
				return err
			}
		}
		// Disk removal: let the guest go of any disk being removed BEFORE its volume is detached -
		// the same shape as the drain above, and for the same reason. This drops the disks' rows, so
		// the specs must be recomputed from the cluster's now-current desired state.
		if err := r.releaseRemovedDisks(ctx, c); err != nil {
			return err
		}
		specs = r.nodeSpecs(c)
		// Converge infra to the desired set (creates added VMs, destroys removed ones; creates,
		// attaches and destroys extra disks' volumes).
		nodes, err := r.prov(c).EnsureNodes(ctx, c.ID, netSpec(c), specs)
		if err != nil {
			return err
		}
		c.Nodes = toDomainNodes(specs, nodes)
		observeDisks(c, nodes)
		// Join any newly-added workers (idempotent: already-joined nodes are skipped).
		if err := r.Cfg.JoinWorkers(ctx, c); err != nil {
			return err
		}
		// Format and mount whatever is now attached.
		if err := r.mountNodeDisks(ctx, c); err != nil {
			return err
		}
		if err := r.reconcileAddons(ctx, c); err != nil {
			return err
		}
		if err := r.reconcileMonitoringWiring(ctx, c); err != nil {
			return err
		}
		if err := r.reconcileGatewayWiring(ctx, c); err != nil {
			return err
		}
		// With the gateway answering on the reserved address, publish the cluster's apps wildcard.
		if err := r.reconcileDNSWiring(ctx, c); err != nil {
			return err
		}
		// An external-secrets add-on installed by this very update wires Vault here.
		if err := r.reconcileVaultWiring(ctx, c); err != nil {
			return err
		}
		// A cluster that predates the registry integration gets its project on its next update.
		if err := r.reconcileRegistryWiring(ctx, c); err != nil {
			return err
		}
		// A disk attached (or removed) by this very update has to reach Longhorn, which is why the
		// storage marker is a fingerprint rather than a latch.
		if err := r.reconcileStorageWiring(ctx, c); err != nil {
			return err
		}
		c.Phase = domain.PhaseReady
		// Only call the update done once every disk is actually mounted. A disk whose identity the
		// infrastructure hasn't reported yet is NOT converged - nothing has formatted it - so leave
		// observed_generation behind and let the level-triggered loop come back for it, exactly as
		// the WorkersReady path does when the node set moved during bring-up. Catching
		// observed_generation up here instead would park the cluster at Ready with a permanently
		// pending disk: a Ready, converged cluster is never reconciled again, so nothing would ever
		// mount it.
		if disksConverged(c) {
			c.ObservedGeneration = c.Generation
			r.emit(c.ID, "info", "reconciler", "update complete")
		} else {
			r.emit(c.ID, "info", "reconciler", "waiting for the infrastructure to report a disk's identity - retrying")
		}

	case domain.PhaseDeleting:
		// Withdraw the cluster's DNS before the infrastructure goes away - see releaseDNS.
		if err := r.releaseDNS(ctx, c); err != nil {
			return err
		}
		// Tear down the cluster's Vault path before the infrastructure goes away - see releaseVault.
		if err := r.releaseVault(ctx, c); err != nil {
			return err
		}
		// And its registry project, for the sharper version of the same reason: the project is named
		// after the cluster, and the name is reusable - see releaseRegistry.
		if err := r.releaseRegistry(ctx, c); err != nil {
			return err
		}
		if err := r.prov(c).DestroyCluster(ctx, c.ID); err != nil {
			return err
		}
		now := time.Now()
		c.DeletedAt = &now
		c.Phase = domain.PhaseDeleted
		r.emit(c.ID, "info", "reconciler", "cluster deleted")
	}

	// Persist with a generation guard so a delete (or edit) that landed while this - possibly
	// minutes-long - phase step ran isn't clobbered by our now-stale copy. A delete flips the cluster
	// to Deleting and bumps the generation; without the guard our write would resurrect it and
	// creation would run to completion regardless. On a superseded/gone row we drop this transition
	// (every step is idempotent) and let the next tick re-read the fresh desired state - the Deleting
	// phase then tears the infrastructure down.
	if err := r.Store.UpdateClusterUnlessSuperseded(c); err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			r.Log.Info("reconcile: desired state changed under reconciler - abandoning stale write, will re-read",
				"cluster", c.Name, "phase", c.Phase)
			return nil
		}
		return err
	}
	// Once a cluster is fully converged (Ready and observed generation caught up), close out any
	// action-history operations up to that generation. Idempotent: only in-progress rows change,
	// and reconcileOne runs for a converged cluster only on the tick it settles.
	if c.Phase == domain.PhaseReady && c.ObservedGeneration == c.Generation {
		if err := r.Store.CompleteOperations(c.ID, c.Generation, time.Now()); err != nil {
			r.Log.Error("complete operations", "cluster", c.Name, "err", err)
		}
	}
	return nil
}

// reconcileAddons converges the cluster's add-on set: it installs any add-on not yet
// "installed" and uninstalls those marked "removing" (then drops them from the list). Both
// Helm operations are idempotent. The admin kubeconfig is fetched and decrypted at most once,
// and only if there's actual work. On the first failure it returns (leaving the remaining
// work for the next tick); progress already made is re-applied idempotently.
func (r *Reconciler) reconcileAddons(ctx context.Context, c *domain.Cluster) error {
	var kubeconfig []byte
	fetched := false
	ensureKubeconfig := func() error {
		if fetched {
			return nil
		}
		kc, err := r.getSecret(c.ID, domain.SecretKubeconfig)
		if err != nil {
			return fmt.Errorf("addons: kubeconfig unavailable: %w", err)
		}
		kubeconfig, fetched = kc, true
		return nil
	}

	// next accumulates the add-ons that survive this pass. On a mid-loop error we splice the
	// unprocessed remainder (c.Addons[i:]) back on, so nothing the reconciler hasn't looked
	// at is silently dropped; the failed step is retried idempotently next tick.
	next := make([]domain.Addon, 0, len(c.Addons))
	for i := range c.Addons {
		ad := c.Addons[i]
		switch ad.Phase {
		case "removing":
			if err := ensureKubeconfig(); err != nil {
				c.Addons = append(next, c.Addons[i:]...)
				return err
			}
			if err := r.Addons.Uninstall(ctx, c, ad, kubeconfig); err != nil {
				c.Addons = append(next, c.Addons[i:]...)
				return err
			}
			r.emit(c.ID, "info", "addon", fmt.Sprintf("removed add-on %q", ad.Name))
			// dropped: not appended to next
		case "installed":
			next = append(next, ad)
		default: // "pending", "updating" (values changed), or unset - (re)install idempotently
			if err := ensureKubeconfig(); err != nil {
				c.Addons = append(next, c.Addons[i:]...)
				return err
			}
			updating := ad.Phase == "updating"
			if err := r.Addons.Install(ctx, c, ad, kubeconfig); err != nil {
				c.Addons = append(next, c.Addons[i:]...)
				return err
			}
			ad.Phase = "installed"
			verb := "installed"
			if updating {
				verb = "updated values for"
			}
			r.emit(c.ID, "info", "addon", fmt.Sprintf("%s add-on %q", verb, ad.Name))
			next = append(next, ad)
		}
	}
	c.Addons = next
	return nil
}

// reconcileUpgrade advances a cluster one supersedes hop toward its TargetBundle. It diffs the
// current bundle against the next hop and routes each changed component to its strategy:
//   - OS change      → rolling node replacement, one node per tick (recreate from the new golden
//     image, which also carries the new Kubernetes); returns mid-hop while nodes
//     remain, so progress is one node per invocation.
//   - Kubernetes     → in-place kubeadm upgrade of every node.
//   - CNI / add-ons  → helm upgrade to the newly pinned versions.
//
// Every step is idempotent (guarded playbooks, helm upgrade --install, the per-node Image marker),
// so a failed hop is simply retried. observed_generation is only caught up once Bundle reaches
// TargetBundle, so a multi-hop promotion keeps needing work until it arrives.
func (r *Reconciler) reconcileUpgrade(ctx context.Context, c *domain.Cluster) error {
	hop, ok := r.Catalog.NextUpgrade(c.Bundle)
	if !ok {
		// Nothing to promote to (target unreachable or already there): settle back to Ready.
		c.Phase = domain.PhaseReady
		c.TargetBundle = ""
		c.ObservedGeneration = c.Generation
		return nil
	}
	from, err := r.Catalog.Resolve(c.Bundle)
	if err != nil {
		return err
	}
	to, err := r.Catalog.Resolve(hop.Name)
	if err != nil {
		return err
	}
	diff := catalog.DiffResolved(from, to)
	targetImage := catalog.GoldenImageNameFor(c.InfraProvider(), to.OS.Name, to.Kubernetes)

	switch {
	case diff.OSChanged:
		// Rolling replacement: recreate nodes onto the new-OS golden image one at a time. Return
		// while any node is still on the old image so each tick makes one node's worth of progress.
		if !allNodesOnImage(c.Nodes, targetImage) {
			if err := r.rollOneNode(ctx, c, targetImage); err != nil {
				return err
			}
			return nil // stay in Upgrading; more nodes to replace
		}
		// All nodes now run the new image (new OS + new Kubernetes baked in): fall through to finalize.
	case diff.K8sChanged:
		r.emit(c.ID, "info", "ansible", fmt.Sprintf("upgrading Kubernetes %s → %s (in place)", c.K8sVersion, to.Kubernetes))
		if err := r.Cfg.UpgradeKubernetes(ctx, c, to.Kubernetes); err != nil {
			return err
		}
	}

	if err := r.finalizeHop(ctx, c, hop.Name, to, diff); err != nil {
		return err
	}

	// A Kubernetes bump (`kubeadm upgrade apply` renews all certs) or an OS roll (replaced nodes
	// rejoin with freshly-issued certs) reissues the control-plane PKI, so the observed expiry is now
	// stale. Drop it so the next Ready tick re-observes the extended validity - and, for a sole-CP
	// etcd restore that put back still-expiring certs, catches that they remain due and renews.
	if diff.K8sChanged || diff.OSChanged {
		c.CertNotAfter = nil
	}

	// Re-wire monitoring if this hop's CNI upgrade or control-plane node roll cleared MonitoringWired
	// (a no-op otherwise - the marker gates it). Without this an OS/CNI upgrade would silently lose the
	// control-plane/CNI scrape wiring, since the Ready path never re-runs it.
	if err := r.reconcileMonitoringWiring(ctx, c); err != nil {
		return err
	}

	c.Phase = domain.PhaseReady
	if c.Bundle == c.TargetBundle {
		c.TargetBundle = ""
		c.Status = fmt.Sprintf("upgraded to %s (k8s %s)", c.Bundle, c.K8sVersion)
		if r.nodesConverged(c) {
			c.ObservedGeneration = c.Generation
		}
		r.emit(c.ID, "info", "reconciler", fmt.Sprintf("upgrade to %s complete", c.Bundle))
	} else {
		// More hops to go: leave observed_generation behind so Ready→Upgrading fires again.
		r.emit(c.ID, "info", "reconciler", fmt.Sprintf("promoted to %s - continuing toward %s", c.Bundle, c.TargetBundle))
	}
	return nil
}

// finalizeHop advances the cluster's version provenance to the just-applied hop and converges the
// CNI and add-ons to their newly pinned versions via helm (idempotent upgrade --install).
func (r *Reconciler) finalizeHop(ctx context.Context, c *domain.Cluster, bundleName string, to catalog.ResolvedBundle, diff catalog.BundleDiff) error {
	c.Bundle = bundleName
	c.OSImage = to.OS.Name
	c.K8sVersion = to.Kubernetes
	c.CNI = to.CNI.Name
	c.CNIVersion = to.CNI.Version

	if diff.CNIChanged {
		if err := r.Cfg.InstallCNI(ctx, c); err != nil {
			return err
		}
		r.emit(c.ID, "info", "ansible", fmt.Sprintf("upgraded CNI %q to %s", c.CNI, c.CNIVersion))
		// The CNI helm upgrade re-renders its release without the ServiceMonitor (cni.yml only enables
		// it under EnsureCNIMetrics), so the wiring must be re-applied. reconcileUpgrade re-runs
		// reconcileMonitoringWiring after this.
		c.MonitoringWired = false
	}

	// Bump the versions of add-ons this cluster actually runs to the hop's pinned versions, marking
	// changed ones pending so reconcileAddons helm-upgrades them. Add-ons the new bundle no longer
	// pins are left as-is (the user still wants them).
	pinned := make(map[string]string, len(to.Addons))
	for _, a := range to.Addons {
		pinned[a.Name] = a.Version
	}
	for i := range c.Addons {
		ad := &c.Addons[i]
		if ad.Phase == "removing" {
			continue
		}
		if v, ok := pinned[ad.Name]; ok && v != ad.Version {
			ad.Version = v
			ad.Phase = "pending"
		}
	}
	return r.reconcileAddons(ctx, c)
}

// rollOneNode replaces exactly one node still on the old golden image with a fresh VM cloned from
// targetImage, workers first then control planes. One node per call gives visible rolling progress
// and keeps each step retryable.
func (r *Reconciler) rollOneNode(ctx context.Context, c *domain.Cluster, targetImage string) error {
	node, ok := nextNodeToRoll(c.Nodes, targetImage)
	if !ok {
		return nil
	}
	return r.replaceNode(ctx, c, node, targetImage, false)
}

// replaceNode destroys and rebuilds ONE node onto image, taking it out of the cluster first and
// rejoining it after: drain/remove (or, for a sole control plane, back up), recreate the VM, rejoin.
//
// It serves two callers that ask the same question for different reasons, which is exactly why it is
// one function. A rolling OS upgrade replaces a node because its IMAGE CHANGED; automatic repair
// replaces one because the NODE IS BROKEN and its image is fine. Everything else - the HA-vs-sole
// control-plane branching, the etcd member removal, the per-node image preservation, the extra-disk
// re-adoption - is identical, and a repair path that reimplemented it would be a second, far less
// exercised way to destroy a node.
//
// `force` is the difference. An upgrade changes the node's image, which every backend treats as
// ForceNew, so an ordinary converge rebuilds the VM on its own. A repair rebuilds onto the SAME
// image, so nothing about desired state changes and a converge would do nothing at all - the
// provisioner has to be told explicitly (provision.NodeReplacer).
func (r *Reconciler) replaceNode(ctx context.Context, c *domain.Cluster, node domain.Node, targetImage string, force bool) error {
	// Preflight: never drain/remove a node we can't actually rebuild onto the target image. Without
	// the golden image the OS can't change (there is no apt fallback for the OS), so proceeding
	// would delete the node from the cluster and then fail to rejoin it - corrupting the cluster while
	// the store/UI wrongly reported the new image. Fail loudly instead; it retries idempotently
	// once the image is built. It matters for a repair too: rebuilding onto an image that has since
	// been deleted from the pool would turn a broken node into a missing one.
	if checker, ok := provision.AsImageChecker(r.prov(c)); ok {
		if err := checker.ImageAvailable(targetImage); err != nil {
			return fmt.Errorf("cannot replace %s onto %s: %w", node.VMName, targetImage, err)
		}
	}
	// There is no longer a disk-preservation preflight here: every backend now keeps a node's extra
	// disks in resources INDEPENDENT of its VM (a libvirt_volume, a vsphere_virtual_disk, a volume on
	// the Proxmox disk-owner VM), so replacing the VM preserves them and no node has to be refused.
	// See provision.NodeReplacer.
	isCP := node.Role == domain.RoleControlPlane
	if isCP {
		// The replacement control-plane VM boots from a golden image whose kubeadm manifests bind the
		// scheduler/controller-manager/etcd metrics to loopback again, so the control-plane scrape
		// wiring must be re-applied once the roll finishes (reconcileUpgrade re-runs it after the hop).
		c.MonitoringWired = false
	}
	// A control plane's identity (certs + etcd member) is keyed on its IP, so how we replace it
	// depends on the topology:
	//   - worker            → drain/remove, then rejoin after recreate.
	//   - control plane, HA  → remove its etcd member (from a surviving CP), then join the new one.
	//   - control plane, sole → back up etcd + /etc/kubernetes, recreate (the pinned MAC hands back
	//                           the SAME IP), then restore state onto the new VM.
	soleCP := isCP && !c.HA()

	// Pre-recreate: get the departing node out of the cluster / capture its state.
	switch {
	case soleCP:
		r.emit(c.ID, "info", "ansible", fmt.Sprintf("backing up sole control plane %s before OS replacement", node.VMName))
		if err := r.Cfg.BackupControlPlane(ctx, c, node); err != nil {
			return err
		}
	case isCP:
		r.emit(c.ID, "info", "ansible", fmt.Sprintf("replacing control plane %s onto %s", node.VMName, targetImage))
		if err := r.Cfg.RemoveControlPlane(ctx, c, node); err != nil {
			return err
		}
	default:
		r.emit(c.ID, "info", "ansible", fmt.Sprintf("replacing worker %s onto %s", node.VMName, targetImage))
		if err := r.Cfg.RemoveWorkers(ctx, c, []domain.Node{node}); err != nil {
			return err
		}
	}

	// Recreate the VM from the target image, preserving every other node's current image so only this
	// node is replaced.
	nodes, err := r.recreateNodeVM(ctx, c, node, targetImage, force)
	if err != nil {
		return err
	}

	// Post-recreate: bring the replaced node back (idempotent: skipped if already in the cluster).
	switch {
	case soleCP:
		r.emit(c.ID, "info", "ansible", fmt.Sprintf("restoring control plane %s onto %s", node.VMName, targetImage))
		return r.Cfg.RestoreControlPlane(ctx, c, nodeByName(c.Nodes, node.VMName))
	case isCP:
		return r.Cfg.JoinControlPlane(ctx, c, nodeByName(c.Nodes, node.VMName))
	default:
		if err := r.Cfg.JoinWorkers(ctx, c); err != nil {
			return err
		}
		// The rebuilt node booted a brand-new ROOT disk, so its /etc/fstab knows nothing about its
		// extra disks - but the disks themselves are untouched: their volumes still exist, still
		// carry their LVM metadata and their filesystem, and the module re-attached them when it
		// re-created the domain.
		//
		// Re-running the role re-ADOPTS them rather than rebuilding them: every step finds its
		// object already present and skips - crucially including mkfs, which is guarded by a blkid
		// check on the LV. So the node comes back with its data intact and its mounts restored.
		// This is what makes a rolling OS replacement non-destructive for node-local storage.
		observeDisks(c, nodes)
		return r.mountNodeDisks(ctx, c)
	}
}

// recreateNodeVM destroys and re-creates ONE node's VM, leaving every other node exactly as it is,
// and returns the provisioner's fresh view of the cluster's nodes.
//
// Preserving the other nodes' CURRENT images (rather than letting them all take the target) is what
// makes a rolling upgrade roll: without it the first node's replacement would rewrite every node's
// spec and the backend would replace the whole cluster at once.
//
// The `force` branch is the one genuinely new mechanism automatic repair needed. EnsureNodes is a
// converge, so a VM that exists but is broken already matches its spec and nothing happens; a repair
// rebuilds onto the same image, so there is no diff to drive it. provision.NodeReplacer is the
// explicit "this resource is unhealthy, rebuild it" that has no declarative expression. A backend
// without the capability fails here rather than silently doing nothing - a repair that reports
// success without touching anything would let the ladder believe it had tried.
func (r *Reconciler) recreateNodeVM(ctx context.Context, c *domain.Cluster, node domain.Node, targetImage string, force bool) ([]provision.ProvisionedNode, error) {
	specs := r.nodeSpecs(c)
	current := make(map[string]string, len(c.Nodes))
	for _, n := range c.Nodes {
		current[n.VMName] = n.Image
	}
	for i := range specs {
		if img := current[specs[i].VMName]; img != "" {
			specs[i].Image = img
		}
		if specs[i].VMName == node.VMName {
			specs[i].Image = targetImage
		}
	}
	if force {
		replacer, ok := provision.AsNodeReplacer(r.prov(c))
		if !ok {
			return nil, fmt.Errorf("provider %s cannot rebuild a single node in place", c.InfraProvider())
		}
		// Before EnsureNodes, not instead of it: ReplaceNode destroys and re-creates this node's VM
		// and root volume, and the converge that follows brings the workspace back to a fully
		// consistent state and hands back the observed addressing (which a re-created VM may have
		// re-learned, e.g. a DHCP lease reclaimed via its pinned MAC).
		if err := replacer.ReplaceNode(ctx, c.ID, node.VMName); err != nil {
			return nil, fmt.Errorf("rebuild %s: %w", node.VMName, err)
		}
	}
	nodes, err := r.prov(c).EnsureNodes(ctx, c.ID, netSpec(c), specs)
	if err != nil {
		return nil, err
	}
	c.Nodes = toDomainNodes(specs, nodes)
	return nodes, nil
}

// allNodesOnImage reports whether every node is already running the given golden image.
func allNodesOnImage(nodes []domain.Node, image string) bool {
	for _, n := range nodes {
		if n.Image != image {
			return false
		}
	}
	return len(nodes) > 0
}

// nextNodeToRoll returns the next node to replace during a rolling OS upgrade - workers before
// control planes (control planes are the riskiest, rolled last), skipping nodes already replaced.
func nextNodeToRoll(nodes []domain.Node, targetImage string) (domain.Node, bool) {
	for _, n := range nodes {
		if n.Role == domain.RoleWorker && n.Image != targetImage {
			return n, true
		}
	}
	for _, n := range nodes {
		if n.Role == domain.RoleControlPlane && n.Image != targetImage {
			return n, true
		}
	}
	return domain.Node{}, false
}

// nodeByName returns the node with the given VM name (zero value if absent).
func nodeByName(nodes []domain.Node, name string) domain.Node {
	for _, n := range nodes {
		if n.VMName == name {
			return n
		}
	}
	return domain.Node{}
}

// netSpec is the node network the cluster's VMs attach to. Every value comes off the cluster
// row - the network was resolved once, at admission (see internal/app), so the loop stays
// level-triggered and a re-provision can't land a cluster on a different network than the one
// it was admitted onto. kvm: a dedicated isolated NAT bridge per cluster (internal/netpool,
// infra/libvirt). vsphere: the operator's shared portgroup, in dhcp or static addressing mode
// (infra/vsphere).
func netSpec(c *domain.Cluster) provision.NetworkSpec {
	// vSphere and Proxmox are shared-network providers: the cluster carries the operator's network
	// (a portgroup / a bridge) plus the ip_mode and, in static mode, gateway/DNS. KVM is the odd one
	// out - each cluster owns a dedicated NAT network.
	switch c.InfraProvider() {
	case domain.ProviderVSphere, domain.ProviderProxmox:
		var dns []string
		for _, s := range strings.Split(c.NetDNS, ",") {
			if s = strings.TrimSpace(s); s != "" {
				dns = append(dns, s)
			}
		}
		return provision.NetworkSpec{
			CIDR: c.NetworkCIDR, Mode: c.IPMode, Name: c.NetworkName,
			Gateway: c.NetGateway, DNS: dns, VIP: c.APIVIP, ClusterName: c.Name,
			LoadBalancerIP: c.LoadBalancerIP,
		}
	default: // kvm
		return provision.NetworkSpec{CIDR: c.NetworkCIDR, Mode: "nat", ClusterName: c.Name}
	}
}

// nodeSpecs is the cluster's desired VMs. The node set - names, roles, owning pool and per-node
// resources - comes wholesale from domain.DesiredNodes, so a node's size is its POOL's size (control
// planes take the cluster's own). Everything added here is infrastructure detail the domain layer
// has no business knowing: the golden image and the pre-allocated address.
func (r *Reconciler) nodeSpecs(c *domain.Cluster) []provision.NodeSpec {
	// Golden image per (OS, k8s) - a qcow2 volume on kvm, a VM template on vsphere.
	image := catalog.GoldenImageNameFor(c.InfraProvider(), c.OSImage, c.K8sVersion)
	desired := domain.DesiredNodes(c)
	specs := make([]provision.NodeSpec, 0, len(desired))
	for _, d := range desired {
		specs = append(specs, provision.NodeSpec{
			VMName: d.VMName, Role: d.Role, Pool: d.Pool,
			CPUs: d.Spec.CPUs, MemMB: d.Spec.MemMB, DiskGB: d.Spec.DiskGB, Image: image,
			// Pre-allocated address (vsphere static mode); empty means the network's DHCP
			// assigns it. Keyed by VM name, so a re-created node keeps its IP.
			IP: c.StaticIPs[d.VMName],
			// Extra disks this node should have. A disk still in the "removing" phase is
			// deliberately still listed: the provisioner must keep its volume attached until the
			// guest has let go of it, and only reconcileNodeDisks - after the unmount has actually
			// run - drops the row and lets the next apply destroy it.
			Disks: diskSpecs(c, d.VMName),
		})
	}
	return specs
}

// diskSpecs is one node's extra disks as the provisioner wants them, in stable name order (which on
// vsphere fixes each disk's SCSI unit for life - see that module).
func diskSpecs(c *domain.Cluster, vmName string) []provision.DiskSpec {
	disks := domain.DisksFor(c, vmName)
	if len(disks) == 0 {
		return nil
	}
	out := make([]provision.DiskSpec, 0, len(disks))
	for _, d := range disks {
		out = append(out, provision.DiskSpec{Name: d.Name, SizeGB: d.SizeGB, WWN: d.WWN})
	}
	return out
}

// nodesConverged reports whether the provisioned nodes exactly match the desired node set
// derived from the cluster's current size + worker count. Used to detect a desired-state
// change that landed during bring-up.
func (r *Reconciler) nodesConverged(c *domain.Cluster) bool {
	specs := r.nodeSpecs(c)
	if len(specs) != len(c.Nodes) {
		return false
	}
	have := make(map[string]bool, len(c.Nodes))
	for _, n := range c.Nodes {
		have[n.VMName] = true
	}
	for _, s := range specs {
		if !have[s.VMName] {
			return false
		}
	}
	return true
}

// removedWorkers returns the current worker nodes that are absent from the desired specs
// (i.e. workers being scaled away). Control-plane nodes are never removed here.
func removedWorkers(current []domain.Node, desired []provision.NodeSpec) []domain.Node {
	want := make(map[string]struct{}, len(desired))
	for _, s := range desired {
		want[s.VMName] = struct{}{}
	}
	var removed []domain.Node
	for _, n := range current {
		if n.Role != domain.RoleWorker {
			continue
		}
		if _, keep := want[n.VMName]; !keep {
			removed = append(removed, n)
		}
	}
	return removed
}

// releaseRemovedDisks lets the GUEST go of every disk the user has asked to remove - unmount, fstab,
// vgremove - and then drops their rows, which is what makes the following EnsureNodes detach and
// destroy their volumes.
//
// The ordering is the whole safety property, and it mirrors the scale-down above exactly: drain the
// node before destroying its VM; release the disk before destroying its volume. Detaching first
// would leave the node with a mount over a device that no longer exists - I/O errors for anything
// writing there, and a hang at next boot were it not for the fstab `nofail`.
//
// Callers MUST recompute the node specs afterwards: the desired disk set is derived from
// c.NodeDisks, and this changes it.
//
// Idempotent: an already-unmounted disk unmounts to a no-op, and a row already dropped is simply not
// in the list.
func (r *Reconciler) releaseRemovedDisks(ctx context.Context, c *domain.Cluster) error {
	var removing []domain.NodeDisk
	for _, d := range c.NodeDisks {
		if d.Phase == domain.DiskPhaseRemoving {
			removing = append(removing, d)
		}
	}
	if len(removing) == 0 {
		return nil
	}
	r.emit(c.ID, "info", "ansible", fmt.Sprintf("releasing %d disk(s) before detach", len(removing)))
	// Longhorn first, and this ordering is load-bearing in the same way the whole function is. A disk
	// registered as pool capacity holds volume REPLICAS; unmounting it under Longhorn degrades every
	// volume that had one there, and loses any volume whose only replica lived on it. Asking Longhorn
	// to evict moves them while they are still readable - so the guest teardown below, and the detach
	// after it, are the last two steps rather than the first.
	if err := r.Cfg.EvictLonghornDisks(ctx, c, removing); err != nil {
		return err
	}
	if err := r.Cfg.RemoveNodeDisks(ctx, c, removing); err != nil {
		return err
	}
	kept := make([]domain.NodeDisk, 0, len(c.NodeDisks))
	for _, d := range c.NodeDisks {
		if d.Phase != domain.DiskPhaseRemoving {
			kept = append(kept, d)
		}
	}
	c.NodeDisks = kept
	// The eviction above already dropped these disks from their nodes' CRs, so Longhorn's view now
	// matches the disks that remain - record that, rather than leaving a stale fingerprint the next
	// wiring pass would compare against.
	c.StorageWired = domain.StorageFingerprint(c)
	return nil
}

// mountNodeDisks formats and mounts every attached extra disk, and marks as "attached" each disk the
// provisioner has now reported an identity for. Idempotent, and a no-op for a cluster with no disks.
func (r *Reconciler) mountNodeDisks(ctx context.Context, c *domain.Cluster) error {
	if err := r.Cfg.EnsureNodeDisks(ctx, c); err != nil {
		return err
	}
	for i, d := range c.NodeDisks {
		if d.DeviceID != "" && d.Phase != domain.DiskPhaseAttached {
			c.NodeDisks[i].Phase = domain.DiskPhaseAttached
			r.emit(c.ID, "info", "reconciler", fmt.Sprintf("disk %q on %s mounted at %s", d.Name, d.VMName, d.MountPath))
		}
	}
	return nil
}

// disksConverged reports whether every extra disk is actually in service - created, attached, and
// formatted/mounted (domain.DiskPhaseAttached).
//
// The case this exists for is vsphere: vCenter mints the VMDK's UUID and does not report it on the
// tick that creates the disk, so the disk's DeviceID is empty then, mountNodeDisks correctly refuses
// to format a device it cannot identify, and the disk is still "pending" when the Updating step
// finishes. Returning false there keeps observed_generation behind so a later tick - by which point
// the identity is readable - completes the job. On kvm the platform picks the WWN itself, so a disk
// is normally converged on the first pass and this is simply true.
func disksConverged(c *domain.Cluster) bool {
	for _, d := range c.NodeDisks {
		if d.Phase != domain.DiskPhaseAttached {
			return false
		}
	}
	return true
}

// observeDisks stamps each extra disk's OBSERVED guest identity onto the cluster, from what the
// provisioner reported. Until a disk has one, nothing will try to format it (see
// ansible.mountableDisks) - which is what stops the platform ever formatting a device it cannot
// positively identify.
func observeDisks(c *domain.Cluster, nodes []provision.ProvisionedNode) {
	byName := make(map[string]provision.ProvisionedNode, len(nodes))
	for _, n := range nodes {
		byName[n.VMName] = n
	}
	for i, d := range c.NodeDisks {
		if id := byName[d.VMName].Disks[d.Name]; id != "" {
			c.NodeDisks[i].DeviceID = id
		}
	}
}

func toDomainNodes(specs []provision.NodeSpec, nodes []provision.ProvisionedNode) []domain.Node {
	byName := make(map[string]provision.ProvisionedNode, len(nodes))
	for _, n := range nodes {
		byName[n.VMName] = n
	}
	out := make([]domain.Node, 0, len(specs))
	for _, s := range specs {
		n := byName[s.VMName]
		out = append(out, domain.Node{
			ID: s.VMName, Role: s.Role, VMName: s.VMName, Pool: s.Pool,
			IP: n.IP, MAC: n.MAC, Phase: "ready", Image: s.Image,
		})
	}
	return out
}

func (r *Reconciler) saveSecret(clusterID string, kind domain.SecretKind, plaintext []byte) error {
	ct, err := r.Secrets.Seal(plaintext)
	if err != nil {
		return err
	}
	return r.Store.SaveSecret(clusterID, kind, ct)
}

func (r *Reconciler) getSecret(clusterID string, kind domain.SecretKind) ([]byte, error) {
	ct, err := r.Store.GetSecret(clusterID, kind)
	if err != nil {
		return nil, err
	}
	return r.Secrets.Open(ct)
}

// ensureViewerKubeconfig applies the cluster's read-only RBAC and stores the resulting viewer
// kubeconfig under SecretKubeconfigViewer. Idempotent, so it serves both as a bring-up step and as a
// Ready-phase self-heal - the config seam's EnsureViewerKubeconfig converges the in-cluster objects.
func (r *Reconciler) ensureViewerKubeconfig(ctx context.Context, c *domain.Cluster) error {
	kc, err := r.Cfg.EnsureViewerKubeconfig(ctx, c)
	if err != nil {
		return fmt.Errorf("viewer kubeconfig: %w", err)
	}
	if err := r.saveSecret(c.ID, domain.SecretKubeconfigViewer, kc); err != nil {
		return err
	}
	r.emit(c.ID, "info", "reconciler", "read-only viewer kubeconfig ready")
	return nil
}

// reconcileCerts is the Ready-tick half of automatic certificate rotation. It runs only for a Ready,
// converged, cert-due cluster (that's the sole reason such a cluster is in the work set). It observes
// the control-plane certificate expiry the first time it's seen - the one-time backfill for clusters
// that predate this feature, stamped so they stop re-qualifying once known - and promotes the cluster
// into PhaseRenewingCerts when expiry has actually fallen within the renewal window. Splitting observe
// from renew keeps the cheap, always-safe read out of the mutating step.
func (r *Reconciler) reconcileCerts(ctx context.Context, c *domain.Cluster) error {
	if c.CertNotAfter == nil {
		exp, err := r.Cfg.CertExpiry(ctx, c)
		if err != nil {
			return fmt.Errorf("observe cert expiry: %w", err)
		}
		c.CertNotAfter = &exp
		r.emit(c.ID, "info", "reconciler", fmt.Sprintf("observed control-plane certificate expiry: %s", exp.Format("2006-01-02")))
	}
	if c.CertNotAfter.Before(r.certRenewCutoff()) {
		r.emit(c.ID, "info", "reconciler", "control-plane certificates approaching expiry - renewing")
		c.Phase = domain.PhaseRenewingCerts
	}
	return nil
}

// reconcileCertRenewal is PhaseRenewingCerts: renew the kubeadm-managed control-plane certs on every
// control plane and re-seal the stored admin kubeconfig with the freshly-issued one - every
// worker-side seam reads it, so a renewed node cert with a stale stored kubeconfig would break them
// once the old one lapses. The viewer kubeconfig rides a non-expiring ServiceAccount token bound to
// the unchanged CA, so it needs no re-mint. Idempotent: RenewCerts just resets the clock, so a
// retried step re-converges.
func (r *Reconciler) reconcileCertRenewal(ctx context.Context, c *domain.Cluster) (err error) {
	// One action-history entry per rotation, closed once below with the new expiry or the error.
	opID := r.recordAutoOp(c.ID, domain.OpCertRenewal, "Control-plane certificate rotation", "")
	var detail string
	defer func() {
		if err != nil {
			detail = "failed: " + err.Error()
		}
		r.completeAutoOp(opID, detail)
	}()

	kubeconfig, notAfter, err := r.Cfg.RenewCerts(ctx, c, r.certRenewCutoff())
	if err != nil {
		return err
	}
	if err := r.saveSecret(c.ID, domain.SecretKubeconfig, kubeconfig); err != nil {
		return err
	}
	c.CertNotAfter = &notAfter
	detail = fmt.Sprintf("valid until %s", notAfter.Format("2006-01-02"))
	r.emit(c.ID, "info", "ansible", "control-plane certificates renewed - "+detail)
	c.Phase = domain.PhaseReady
	return nil
}

func (r *Reconciler) emit(clusterID, level, source, msg string) {
	r.Events.Emit(events.Event{ClusterID: clusterID, Level: level, Source: source, Message: msg})
}
