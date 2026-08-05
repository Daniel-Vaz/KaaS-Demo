// Package app wires the seams together and exposes the cluster service used by both
// the API and the headless worker. Swapping fakes for real implementations (Postgres,
// libvirt/OpenTofu, ansible-runner, Helm) happens here and nowhere else.
package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/addons"
	"github.com/Daniel-Vaz/KaaS-demo/internal/addons/helm"
	"github.com/Daniel-Vaz/KaaS-demo/internal/addons/values"
	"github.com/Daniel-Vaz/KaaS-demo/internal/audit"
	auditkubectl "github.com/Daniel-Vaz/KaaS-demo/internal/audit/kubectl"
	"github.com/Daniel-Vaz/KaaS-demo/internal/auth"
	"github.com/Daniel-Vaz/KaaS-demo/internal/authn"
	"github.com/Daniel-Vaz/KaaS-demo/internal/catalog"
	"github.com/Daniel-Vaz/KaaS-demo/internal/config"
	"github.com/Daniel-Vaz/KaaS-demo/internal/config/ansible"
	"github.com/Daniel-Vaz/KaaS-demo/internal/dns"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/events"
	"github.com/Daniel-Vaz/KaaS-demo/internal/execagent"
	"github.com/Daniel-Vaz/KaaS-demo/internal/health"
	healthkubectl "github.com/Daniel-Vaz/KaaS-demo/internal/health/kubectl"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
	kubekubectl "github.com/Daniel-Vaz/KaaS-demo/internal/kube/kubectl"
	kubeproxy "github.com/Daniel-Vaz/KaaS-demo/internal/kube/proxy"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kvmhost"
	"github.com/Daniel-Vaz/KaaS-demo/internal/metrics"
	"github.com/Daniel-Vaz/KaaS-demo/internal/metrics/kubectl"
	"github.com/Daniel-Vaz/KaaS-demo/internal/monitoring"
	monitoringpromql "github.com/Daniel-Vaz/KaaS-demo/internal/monitoring/promql"
	"github.com/Daniel-Vaz/KaaS-demo/internal/netbox"
	"github.com/Daniel-Vaz/KaaS-demo/internal/netpool"
	"github.com/Daniel-Vaz/KaaS-demo/internal/nodessh"
	nodesshproxy "github.com/Daniel-Vaz/KaaS-demo/internal/nodessh/proxy"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision/proxmox"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision/tofu"
	"github.com/Daniel-Vaz/KaaS-demo/internal/provision/vsphere"
	"github.com/Daniel-Vaz/KaaS-demo/internal/quota"
	"github.com/Daniel-Vaz/KaaS-demo/internal/reconcile"
	"github.com/Daniel-Vaz/KaaS-demo/internal/secrets"
	"github.com/Daniel-Vaz/KaaS-demo/internal/security"
	securitykubectl "github.com/Daniel-Vaz/KaaS-demo/internal/security/kubectl"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell/agent"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell/proxy"
	"github.com/Daniel-Vaz/KaaS-demo/internal/shell/pty"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store/postgres"
	"github.com/Daniel-Vaz/KaaS-demo/internal/tunnel"
	tunnelproxy "github.com/Daniel-Vaz/KaaS-demo/internal/tunnel/proxy"
	"github.com/Daniel-Vaz/KaaS-demo/internal/vault"
)

type App struct {
	Store   store.Store
	Broker  *events.Broker
	Secrets *secrets.Box
	Signer  *auth.Signer // signs/verifies session cookies (see internal/auth)
	// Authn is the directory the portal authenticates against when KAAS_AUTH=ldap, and nil when
	// the deployment uses local accounts alone (the default). Nil is the whole of "local mode" -
	// every directory-shaped code path below is gated on it, so tests and the local deployment
	// share one branch. See internal/authn and buildAuthenticator.
	Authn authn.Authenticator
	// ProviderBudgets is each enabled infrastructure's ceiling, keyed by provider - the KVM host's
	// capacity, the vSphere cluster's budget. There is deliberately no single platform-wide total:
	// capacity doesn't move between backends, so every quota decision (a tenant's grant, an
	// admission check, an admin's own headroom) is scoped to one of these. See platformCapacity
	// and the quota package doc.
	ProviderBudgets map[string]quota.Budget
	// SharedQuota (KAAS_SHARED_QUOTA) turns per-user quota off: instead of an admin carving each
	// infrastructure's ceiling into per-account grants, EVERY account draws its admission budget from
	// the whole ceiling of the provider it's building on (see budgetFor). Stored per-user grants are
	// left untouched but ignored while it is on, so flipping it back restores them. The pool stops
	// being conserved per-account - but the aggregate checkProviderCapacity gate is unchanged, so a
	// backend still can never be physically oversubscribed; it just becomes first-come-first-served
	// across tenants. Grants can't be set while it is on (updateUserLocked rejects them), and the
	// Admin page shows per-user consumption of the shared pool rather than a grant editor.
	SharedQuota bool
	// BundleAddonsOptional (KAAS_BUNDLE_ADDONS_OPTIONAL) lets a create request DESELECT the add-ons
	// that ship with the chosen bundle. Off (the default) a cluster is always born with the whole
	// batteries-included set and an add-on can only be removed once it is Ready - the add-on tab's
	// behaviour is unchanged either way, this governs admission only (see resolveAddons).
	//
	// It exists because the bundle is sized for a real host, not a laptop: kube-prometheus-stack,
	// Longhorn, Cilium, trivy-operator and the gateway pair together outweigh a small cluster's
	// workers on a local KVM host, and installing them all is what tips such a cluster over. The
	// platform tolerates their absence already (every wiring step gates on its add-on being
	// installed - reconcileGatewayWiring, reconcileMonitoringWiring, reconcileVaultWiring, the DNS
	// wiring, and the per-worker Longhorn disk is only provisioned when longhorn is on the cluster),
	// so this only lifts the admission-time lock, it adds no new tolerance.
	BundleAddonsOptional bool
	Catalog              *catalog.Catalog
	Rec                  *reconcile.Reconciler
	Shell                shell.Backend      // in-browser cluster terminal: fake (in-process) or worker-proxy
	NodeSSH              nodessh.Backend    // in-browser node SSH (Nodes tab): fake (in-process) or node-ssh-sandbox proxy
	Kube                 kube.Client        // Workloads page query seam: fake (synthesized) or worker-proxied kubectl
	Monitor              monitoring.Querier // Monitoring page query seam: fake (synthesized) or worker-proxied PromQL
	Security             security.Querier   // Security page query seam: fake (synthesized) or worker-proxied Trivy CRD reads
	Audit                audit.Querier      // Audit tab query seam: fake (synthesized) or worker-proxied apiserver-log reads
	Tunnel               tunnel.Proxier     // Monitoring "Open UI" links: fake (landing page) or worker-proxied HTTP to in-cluster UIs
	Values               values.Provider    // add-on Helm values source for the editor: fake (synthesized) or helm show values
	// Vault is the HashiCorp Vault seam: fake (in-memory) or the real net/http client. On the API it
	// carries only the narrow minter token (VaultSession, the "View in Vault" handoff); the reconciler
	// holds the same interface with the management token for provisioning and access sync. Never nil.
	Vault vault.Manager
	// vaultSettings is the deployment's Vault configuration - the API reads UIURL from it to build the
	// "View in Vault" deep-link. The credential-bearing fields are the worker's concern; the API only
	// needs the browser-facing URL and the mount name.
	vaultSettings vault.Settings
	river         *reconcile.River // non-nil when Postgres is configured (durable job queue)
	kvm           *kvmhost.Host    // where KVM lives; owns the SSH tunnel when it is remote
	// InfraProviders is the enabled infrastructure-provider list (KAAS_INFRA_PROVIDERS, ordered;
	// first = default). vsphere holds that provider's deployment-level network/capacity settings.
	InfraProviders []string
	// sharedNet holds the deployment-level network + budget config for each enabled shared-network
	// provider (vsphere, proxmox), keyed by provider name. kvm is absent - it has no shared network.
	sharedNet map[string]sharedNetSettings
	// dns is the deployment's DNS configuration (KAAS_DNS_*). Admission derives each cluster's
	// domains from it once and stores them on the cluster row; the portal reads them back from
	// there, never from here. Zero value = this deployment publishes no DNS.
	dns dns.Settings
	// userKubeconfigTTL bounds the validity of a per-user client-certificate kubeconfig
	// (KAAS_USER_KUBECONFIG_TTL, default ~1 month). Client certs cannot be revoked before expiry -
	// the same stateless-credential shortcut as the session cookie - so the TTL is the only bound;
	// a user re-downloads (or reopens the shell) to refresh. See userClusterKubeconfig.
	userKubeconfigTTL time.Duration
	// ukcCache memoizes minted per-user kubeconfigs so the interactive seams (shell, Workloads,
	// Storage, scale, download) don't run the CSR dance on every call. Keyed by cluster|user|role and
	// re-derivable on any replica, so it pins no state (see CLAUDE.md, horizontal scaling); entries
	// are re-minted once close to expiry. It holds decrypted client keys in memory, the same
	// sensitivity as the admin kubeconfig the API already handles transiently.
	ukcMu    sync.Mutex
	ukcCache map[string]cachedKubeconfig
	Log      *slog.Logger
}

// cachedKubeconfig is one memoized per-user kubeconfig and its certificate expiry.
type cachedKubeconfig struct {
	kc       []byte
	notAfter time.Time
}

// Authorization / auth sentinels, mapped to HTTP status by the API layer.
var (
	ErrForbidden          = errors.New("forbidden")                    // authenticated but not allowed (e.g. non-admin)
	ErrInvalidCredentials = errors.New("invalid username or password") // login failed
	// ErrRegistrationDisabled is self-registration attempted on a deployment that authenticates
	// against a directory (KAAS_AUTH=ldap). 403, distinct from ErrForbidden so the portal can tell
	// "this deployment doesn't do that" apart from "you're not allowed".
	ErrRegistrationDisabled = errors.New("self-registration is disabled: accounts come from the directory")
	// ErrTooManyAttempts is the login throttle tripping. 429. See internal/app/throttle.go.
	ErrTooManyAttempts = errors.New("too many failed login attempts, try again shortly")
)

// SessionTTL is how long an issued session cookie stays valid.
const SessionTTL = 24 * time.Hour

// New builds the app, selecting the store, provisioner, config manager, add-on manager, and
// job queue from the environment (fake by default; real backends when configured).
func New(log *slog.Logger) (*App, error) {
	box, err := secrets.NewBox(loadKey(log))
	if err != nil {
		return nil, err
	}
	cat, err := catalog.Default()
	if err != nil {
		return nil, err
	}
	st, err := buildStore(log)
	if err != nil {
		return nil, err
	}
	// With Postgres, back the event broker with LISTEN/NOTIFY: in real mode the reconciler
	// runs in a separate worker process, so an in-memory-only broker there would fan out
	// into a void nobody in the API process ever sees (see internal/events).
	var broker *events.Broker
	if pg, ok := st.(*postgres.Store); ok {
		broker = events.NewPostgresBroker(context.Background(), pg.Pool(), log)
	} else {
		broker = events.NewBroker()
	}

	// Where the KVM host is. Local (the default) leaves every seam below exactly as it was; a remote
	// host makes each of them reach the hypervisor and its VMs the long way round - see
	// internal/kvmhost. The SSH tunnel it needs is opened once, here, and shared by every seam (and
	// by the shell sandbox, which is host-networked alongside us).
	kvm, err := kvmhost.FromEnv()
	if err != nil {
		return nil, err
	}
	if err := kvm.Start(context.Background(), log); err != nil {
		return nil, err
	}

	infraProviders, err := enabledProviders()
	if err != nil {
		kvm.Stop()
		return nil, err
	}
	// The remote-KVM machinery (KAAS_KVM_HOST) reroutes Ansible SSH and every kubeconfig through the
	// KVM host - globally, not per cluster - which would misroute a cluster on any other backend.
	// The lab runs local KVM + direct L3 to the vSphere/Proxmox subnets; combining a remote KVM host
	// with a shared-network provider needs per-provider reachability plumbing that v1 deliberately
	// doesn't have. Both providers are reached directly, so the guard covers both.
	sharedNet := make(map[string]sharedNetSettings, 2)
	for _, sp := range []struct {
		name    string
		fromEnv func() (sharedNetSettings, error)
	}{
		{domain.ProviderVSphere, vsphereFromEnv},
		{domain.ProviderProxmox, proxmoxFromEnv},
	} {
		if !slices.Contains(infraProviders, sp.name) {
			continue
		}
		if kvm.Remote() {
			kvm.Stop()
			return nil, fmt.Errorf("KAAS_KVM_HOST and the %s provider cannot be combined: the remote-KVM SSH/SOCKS rerouting is global and would misroute %s clusters", sp.name, sp.name)
		}
		cfg, err := sp.fromEnv()
		if err != nil {
			kvm.Stop()
			return nil, err
		}
		sharedNet[sp.name] = cfg
	}

	// One ceiling per enabled infrastructure - quota is granted and charged per backend.
	providerBudgets := platformCapacity(infraProviders, sharedNet)

	provs, err := buildProvisioners(log, broker, kvm, infraProviders)
	if err != nil {
		kvm.Stop()
		return nil, err
	}
	cfgMgr, err := buildConfigManager(log, broker, kvm)
	if err != nil {
		kvm.Stop()
		return nil, err
	}
	// The site's DNS: which delegated zone this platform hands cluster subdomains out of, and (in
	// the worker) how it writes them. Every process needs the naming half - the API derives a
	// cluster's domains at admission - but only a process that builds a real Registrar needs the
	// credential, which is why the update settings are validated separately.
	dnsSettings, err := dnsFromEnv()
	if err != nil {
		kvm.Stop()
		return nil, err
	}
	addonMgr, err := buildAddonManager(log, broker, cat, kvm, dnsSettings)
	if err != nil {
		kvm.Stop()
		return nil, err
	}
	dnsRegistrar, err := buildDNSRegistrar(log, broker, dnsSettings)
	if err != nil {
		kvm.Stop()
		return nil, err
	}
	vaultSettings, err := vaultFromEnv()
	if err != nil {
		kvm.Stop()
		return nil, err
	}
	vaultMgr, err := buildVaultManager(log, broker, vaultSettings)
	if err != nil {
		kvm.Stop()
		return nil, err
	}
	metricsCol, err := buildMetricsCollector(log, kvm)
	if err != nil {
		kvm.Stop()
		return nil, err
	}
	healthChecker, err := buildHealthChecker(log, kvm)
	if err != nil {
		kvm.Stop()
		return nil, err
	}
	shellBackend, err := buildShellBackend(log)
	if err != nil {
		return nil, err
	}
	nodeSSHBackend, err := buildNodeSSHBackend(log)
	if err != nil {
		return nil, err
	}
	kubeClient, err := buildKubeClient(log)
	if err != nil {
		return nil, err
	}
	monitorQuerier, err := buildMonitoringQuerier(log)
	if err != nil {
		return nil, err
	}
	securityQuerier, err := buildSecurityQuerier(log)
	if err != nil {
		return nil, err
	}
	auditQuerier, err := buildAuditQuerier(log)
	if err != nil {
		return nil, err
	}
	tunnelProxier, err := buildTunnel(log)
	if err != nil {
		return nil, err
	}
	valuesProvider, err := buildAddonValuesProvider()
	if err != nil {
		return nil, err
	}
	// The directory, when this deployment authenticates against one. Built in every process that
	// calls New - but only the API ever authenticates a user, so the worker's compose/Helm env
	// deliberately omits KAAS_AUTH and it lands here as nil (see internal/app/authn.go). That keeps
	// the bind password out of the privileged worker entirely.
	authenticator, err := buildAuthenticator(log)
	if err != nil {
		kvm.Stop()
		return nil, err
	}
	log.Info("providers selected",
		"infra_providers", strings.Join(infraProviders, ","),
		"shared_quota", envBool("KAAS_SHARED_QUOTA", false),
		"bundle_addons_optional", envBool("KAAS_BUNDLE_ADDONS_OPTIONAL", false),
		"provisioner", getenv("KAAS_PROVISIONER", "fake"),
		"config", getenv("KAAS_CONFIG", "fake"),
		"addons", getenv("KAAS_ADDONS", "fake"),
		"metrics", getenv("KAAS_METRICS", "fake"),
		"health", getenv("KAAS_HEALTH", "fake"),
		"shell", getenv("KAAS_SHELL", "fake"),
		"node_ssh", getenv("KAAS_NODE_SSH", "fake"),
		"kube", getenv("KAAS_KUBE", "fake"),
		"monitoring", getenv("KAAS_MONITORING", "fake"),
		"security", getenv("KAAS_SECURITY", "fake"),
		"audit", getenv("KAAS_AUDIT", "fake"),
		"tunnel", getenv("KAAS_TUNNEL", "fake"),
		"addon_values", getenv("KAAS_ADDON_VALUES", "auto"),
		"dns", getenv("KAAS_DNS", "fake"),
		"dns_base_domain", getenv("KAAS_DNS_BASE_DOMAIN", "none"),
		"vault", getenv("KAAS_VAULT", "fake"),
		"kvm_host", getenv("KAAS_KVM_HOST", "local"),
		// The ldap fake/real axis isn't logged here - it only means anything in ldap mode, and
		// buildAuthenticator already says which directory it built.
		"auth", getenv("KAAS_AUTH", AuthLocal))

	rec := &reconcile.Reconciler{
		Store:   st,
		Prov:    provs[infraProviders[0]],
		Provs:   provs,
		Cfg:     cfgMgr,
		Addons:  addonMgr,
		DNS:     dnsRegistrar,
		Vault:   vaultMgr,
		Metrics: metricsCol,
		Health:  healthChecker,
		Catalog: cat,
		Secrets: box,
		Events:  broker,
		Log:     log,
		// How often the in-memory tick loop looks for work. Only that loop reads it - with Postgres
		// the queue drives reconciliation instead - so it is a knob for the fake-mode paths: the
		// tests and the browser demo (cmd/demo-wasm), which seeds a fleet at start-up and wants the
		// state machine to walk faster than a human-paced 500ms.
		Interval:        envDuration("KAAS_RECONCILE_INTERVAL", 500*time.Millisecond),
		CertRenewWindow: certRenewWindow(),
		EtcdPolicy:      etcdPolicy(log),
		SnapshotPolicy:  snapshotPolicy(),
		RepairPolicy:    repairPolicy(),
	}
	app := &App{
		Store:           st,
		Broker:          broker,
		Secrets:         box,
		Signer:          auth.NewSigner(os.Getenv("KAAS_SECRET_KEY")),
		Authn:           authenticator,
		Catalog:         cat,
		ProviderBudgets: providerBudgets,
		SharedQuota:     envBool("KAAS_SHARED_QUOTA", false),
		// Off = the bundle's add-ons are locked on at create time (they can still be removed from a
		// Ready cluster). On = the create wizard lets them be deselected, for a host that can't
		// carry the whole batteries-included set. See App.BundleAddonsOptional.
		BundleAddonsOptional: envBool("KAAS_BUNDLE_ADDONS_OPTIONAL", false),
		Rec:                  rec,
		kvm:                  kvm,
		Shell:                shellBackend,
		NodeSSH:              nodeSSHBackend,
		Kube:                 kubeClient,
		Monitor:              monitorQuerier,
		Security:             securityQuerier,
		Audit:                auditQuerier,
		Tunnel:               tunnelProxier,
		Values:               valuesProvider,
		Vault:                vaultMgr,
		vaultSettings:        vaultSettings,
		InfraProviders:       infraProviders,
		sharedNet:            sharedNet,
		dns:                  dnsSettings,
		// ~1 month by default: long enough that a downloaded kubeconfig keeps working for a while,
		// short enough to bound an un-revokable credential (see App.userKubeconfigTTL).
		userKubeconfigTTL: envDuration("KAAS_USER_KUBECONFIG_TTL", 30*24*time.Hour),
		Log:               log,
	}
	// With Postgres, drive reconciliation through River's durable job queue - but only in a process
	// that will actually reconcile. The API replicas set KAAS_DISABLE_RECONCILER (the worker owns the
	// loop in real mode), and a client they never start is not free: NewRiver runs River's schema
	// migrations, so every API replica would pile another migrator onto the same database at boot.
	if pg, ok := st.(*postgres.Store); ok && !reconcilerDisabled() {
		riv, err := reconcile.NewRiver(context.Background(), pg.Pool(), rec, log, reconcileJobTimeout(log))
		if err != nil {
			return nil, err
		}
		app.river = riv
	}
	// Seed the admin account and take ownership of any pre-tenancy clusters. Idempotent, so it is
	// harmless when both the api and worker processes call New against the same database.
	if err := app.ensureAdmin(); err != nil {
		return nil, fmt.Errorf("seed admin: %w", err)
	}
	// Same idea for the directory's groups: create one per mapping rule so an admin can grant
	// against them before anyone has logged in. No-op unless this process built a directory.
	if err := app.ensureDirectoryGroups(); err != nil {
		return nil, fmt.Errorf("seed directory groups: %w", err)
	}
	return app, nil
}

// reconcilerDisabled reports whether this process must not reconcile (KAAS_DISABLE_RECONCILER=1).
// The API sets it in real mode: the host-networked worker owns the loop, since it is the only
// process that can reach libvirt and the cluster VMs. cmd/api checks the same flag before calling
// StartReconciler.
func reconcilerDisabled() bool { return os.Getenv("KAAS_DISABLE_RECONCILER") == "1" }

// StartReconciler starts driving desired state toward reality: River's durable job queue
// when Postgres is configured, otherwise the in-process tick loop. Non-blocking.
func (a *App) StartReconciler(ctx context.Context) error {
	if a.river != nil {
		return a.river.Start(ctx)
	}
	go a.Rec.Run(ctx)
	return nil
}

// CreateRequest is the user-facing cluster spec (from the wizard / API). Versions are
// bundle-driven: the OS, Kubernetes, and CNI come from the chosen release bundle
// (default: latest supported), not from free-form fields.
type CreateRequest struct {
	Name string `json:"name"`
	// Size is the t-shirt size of the CONTROL-PLANE nodes. Workers are sized per node pool, not here.
	Size string `json:"size"`
	// NodePools are the worker pools to create. A pool named "default" is always ensured (see
	// CreateCluster), so omitting this entirely yields a cluster with one empty default pool that the
	// user can scale later.
	NodePools []domain.NodePool `json:"node_pools,omitempty"`
	HA        bool              `json:"ha"`     // highly-available control plane (3 nodes) vs single node
	Bundle    string            `json:"bundle"` // release bundle; default = latest supported
	// Addons is the catalog add-ons to install. OMITTED (null) means "the bundle's own add-ons" -
	// the batteries-included default. A PRESENT list is the exact selection, so an empty one means
	// no add-ons at all; whether it may drop any of the bundle's own is decided by
	// App.BundleAddonsOptional (see resolveAddons).
	Addons []string `json:"addons"`
	// AddonValues carries per-add-on full Helm values overrides (add-on name -> edited YAML) from the
	// wizard's editor. An add-on absent here installs with the curated catalog defaults; present here,
	// its ValuesOverride is set so the reconciler installs it with `helm -f`. See internal/addons/values.
	AddonValues map[string]string `json:"addon_values,omitempty"`
	// CustomAddons selects add-ons from the actor's visible custom catalogs (see
	// internal/domain.CustomCatalog). Each ref is resolved to a self-contained per-cluster add-on
	// whose chart definition is copied onto the cluster (see resolveCustomAddons).
	CustomAddons []domain.CustomAddonRef `json:"custom_addons,omitempty"`
	// NetworkCIDR is the dedicated node network for this cluster's VMs (kvm only). Empty =
	// auto-allocate a free block from the platform supernet; a value is validated for overlap
	// (see internal/netpool). Rejected on vsphere, whose subnet is deployment configuration.
	NetworkCIDR string `json:"network_cidr"`
	// Provider selects the infrastructure to deploy on. Empty = the deployment's default (the
	// first entry of KAAS_INFRA_PROVIDERS); a value must be an enabled provider.
	Provider string `json:"provider,omitempty"`
	// APIVIP is required for an HA control plane on vsphere in dhcp mode: the user-chosen
	// floating address for keepalived, which must be a free host in the vSphere subnet outside
	// the DHCP pool. Ignored everywhere else (kvm derives it; static mode allocates it).
	APIVIP string `json:"api_vip,omitempty"`
	// LoadBalancerIP is the address reserved for the cluster's default MetalLB L2 pool (and thus the
	// default Envoy Gateway). REQUIRED on a shared-network provider (vsphere/proxmox) in dhcp mode -
	// the external DHCP server owns the pool and the platform can't know a free address outside it, so
	// the user must pick one, exactly like APIVIP for HA. Ignored elsewhere: kvm derives it
	// (netpool.LoadBalancerIP) and static mode allocates it from the operator range.
	LoadBalancerIP string `json:"load_balancer_ip,omitempty"`
	// StorageDiskGB sizes the extra disk every worker is born with, which backs the cluster's default
	// (Longhorn) StorageClass - see domain.DesiredStorageDisks. It is a POINTER so that "unset" and
	// "explicitly zero" are different requests: unset takes domain.DefaultStorageDiskGB, while 0 is a
	// deliberate "provision no storage disks", which is what a user who deselected the longhorn
	// add-on wants. Immutable after creation; capacity is grown by attaching more disks.
	StorageDiskGB *int `json:"storage_disk_gb,omitempty"`
}

// storageDiskGB resolves the create request's storage-disk size: the platform default when the
// caller said nothing, their choice when they did. A cluster without the longhorn add-on gets none
// regardless of what was asked for - the disks exist to back that add-on, and charging a tenant's
// quota for storage nothing will use is worse than ignoring the field.
func storageDiskGB(req CreateRequest, addons []domain.Addon) (int, error) {
	size := domain.DefaultStorageDiskGB
	if req.StorageDiskGB != nil {
		size = *req.StorageDiskGB
	}
	if size == 0 {
		return 0, nil
	}
	if size < domain.MinDiskGB || size > domain.MaxDiskGB {
		return 0, fmt.Errorf("storage disk size must be 0 or between %d and %d GB, got %d",
			domain.MinDiskGB, domain.MaxDiskGB, size)
	}
	for _, ad := range addons {
		if ad.Name == longhornAddon {
			return size, nil
		}
	}
	return 0, nil
}

// haControlPlanes is the control-plane node count for a highly-available cluster (stacked
// etcd needs an odd quorum; 3 is the standard minimum). Single-node clusters use 1.
const haControlPlanes = 3

// ensureDefaultPool guarantees the create request carries a pool named "default", prepending an
// empty one at the cluster's own size if the caller named none. A caller that DOES name a "default"
// pool keeps theirs verbatim - including its size, which is why this can't just prepend blindly.
func ensureDefaultPool(pools []domain.NodePool, size string) []domain.NodePool {
	for _, p := range pools {
		if p.Name == domain.DefaultPoolName {
			return pools
		}
	}
	return append([]domain.NodePool{{Name: domain.DefaultPoolName, Size: size}}, pools...)
}

// CreateCluster validates, resolves versions from the release bundle, enforces the owner's quota,
// and writes desired state owned by actor. The reconciler takes it from Pending to Ready.
func (a *App) CreateCluster(actor *domain.User, req CreateRequest) (*domain.Cluster, error) {
	if actor == nil {
		return nil, ErrForbidden
	}
	if req.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if req.Size == "" {
		req.Size = "small"
	}
	if _, ok := domain.Sizes[req.Size]; !ok {
		return nil, fmt.Errorf("unknown size %q", req.Size)
	}
	// Every cluster is born with a "default" pool, so a user who never thinks about pools still gets
	// the one-pool cluster they expect. It is only a starting shape, not a fixture: once created, the
	// default pool is scaled, and deleted, like any other.
	req.NodePools = ensureDefaultPool(req.NodePools, req.Size)
	if err := domain.ValidateNodePools(req.Name, req.NodePools); err != nil {
		return nil, err
	}
	provider, err := a.resolveProvider(req.Provider)
	if err != nil {
		return nil, err
	}
	req.Provider = provider

	// Resolve the release bundle into concrete, version-pinned components (provenance).
	bundleName := req.Bundle
	if bundleName == "" {
		b, ok := a.Catalog.LatestSupportedBundle()
		if !ok {
			return nil, fmt.Errorf("no supported release bundle in catalog")
		}
		bundleName = b.Name
	}
	rb, err := a.Catalog.Resolve(bundleName)
	if err != nil {
		return nil, err
	}

	controlPlanes := 1
	if req.HA {
		controlPlanes = haControlPlanes
	}

	// Admission (name, quota, node-network IPAM) decides from a snapshot of the live clusters and
	// then writes one more. That read-then-write is only sound if no other admission interleaves
	// with it - with several API replicas, two creates racing on the same snapshot would each pass
	// a check the pair of them jointly violates: the same free CIDR handed to both, or both fitting
	// into the last of a tenant's quota. So the whole decision runs under a platform-wide lock (see
	// store.Store.WithLock). It is a short, VM-free critical section - no provisioning happens here.
	var c *domain.Cluster
	if err := a.Store.WithLock(store.LockAdmission, func() error {
		var err error
		c, err = a.admitCluster(actor, req, rb, controlPlanes)
		return err
	}); err != nil {
		return nil, err
	}

	topo := "single-node control plane"
	if req.HA {
		topo = "HA control plane"
	}
	a.recordOp(actor, c.ID, domain.OpCreate, c.Generation,
		fmt.Sprintf("Created cluster - bundle %s, %s, %d worker(s) across %d node pool(s), %s",
			c.Bundle, c.Size, c.WorkerCount(), len(c.NodePools), topo), poolSummary(c.NodePools))
	return c, nil
}

// poolSummary renders a pool list for an operation's detail line ("default: 2 × small, gpu: 3 ×
// large"). Empty for a cluster with no pools.
func poolSummary(pools []domain.NodePool) string {
	parts := make([]string, 0, len(pools))
	for _, p := range pools {
		parts = append(parts, fmt.Sprintf("%s: %d × %s", p.Name, p.DesiredWorkers, p.Size))
	}
	return strings.Join(parts, ", ")
}

// admitCluster is CreateCluster's critical section: it checks name uniqueness, capacity quota and
// node-network IPAM against the live cluster set, then persists the new cluster. Callers MUST hold
// store.LockAdmission - every read here is only meaningful if the write that follows is the only
// one racing it.
func (a *App) admitCluster(actor *domain.User, req CreateRequest, rb catalog.ResolvedBundle, controlPlanes int) (*domain.Cluster, error) {
	// Node networks are a platform-wide resource - allocate against every cluster. Name uniqueness
	// and quota are per-owner: two tenants may each have a "dev", and each is charged its own quota.
	allClusters, err := a.Store.ListClusters()
	if err != nil {
		return nil, err
	}
	owned, err := a.Store.ListClustersByOwner(actor.ID)
	if err != nil {
		return nil, err
	}
	for _, c := range owned {
		if c.Name == req.Name && c.DeletedAt == nil {
			return nil, fmt.Errorf("cluster %q already exists", req.Name)
		}
	}
	// The candidate is built BEFORE the capacity checks, because both now price a cluster from its
	// whole desired shape - control-plane size plus every pool's own size (quota.ClusterUsage) - and
	// there is no way to express that as a few scalars.
	c := &domain.Cluster{
		ID:            newID(),
		Name:          req.Name,
		OwnerID:       actor.ID,
		Size:          req.Size,
		NodePools:     req.NodePools,
		ControlPlanes: controlPlanes,
		Provider:      req.Provider,
		PodCIDR:       "10.244.0.0/16",
		SvcCIDR:       "10.96.0.0/12",
		Bundle:        rb.Name,
		OSImage:       rb.OS.Name,
		K8sVersion:    rb.Kubernetes,
		CNI:           rb.CNI.Name,
		CNIVersion:    rb.CNI.Version,
		Phase:         domain.PhasePending,
		Generation:    1,
		CreatedAt:     time.Now(),
	}

	// Add-ons are resolved BEFORE the capacity checks, ahead of where the rest of this function's
	// ordering would suggest, because the platform's storage disks depend on them: the per-worker
	// Longhorn disk is only provisioned when the longhorn add-on is actually on the cluster, and it
	// is real capacity that the checks below have to price.
	addonsToInstall, err := a.resolveAddons(rb, req.Addons, req.AddonValues)
	if err != nil {
		return nil, err
	}
	// Custom-catalog add-ons install after the built-in ones (they may depend on platform add-ons
	// like the monitoring stack's CRDs). Their names must not collide with a built-in add-on.
	taken := make(map[string]bool, len(addonsToInstall))
	for _, ad := range addonsToInstall {
		taken[ad.Name] = true
	}
	customAddons, err := a.resolveCustomAddons(actor, req.CustomAddons, taken)
	if err != nil {
		return nil, err
	}
	c.Addons = append(addonsToInstall, customAddons...)

	// The cluster's default storage: one extra disk per worker, mounted where Longhorn expects its
	// data. Materialized here so the quota checks below charge for it, exactly as they charge for a
	// disk a user attaches later - this is the same NodeDisk mechanism, not a parallel one.
	if c.StorageDiskGB, err = storageDiskGB(req, c.Addons); err != nil {
		return nil, err
	}
	c.NodeDisks = syncStorageDisks(c)

	// Quota is charged against the grant the actor holds on THIS infrastructure, and only their
	// clusters on it count towards it. A tenant with KVM capacity and no vSphere capacity is
	// rejected here for a vSphere cluster however much KVM headroom they have - the two backends
	// are different machines, and a core on one cannot run a VM on the other.
	provider := c.InfraProvider()
	budget, err := a.budgetFor(actor, provider)
	if err != nil {
		return nil, err
	}
	if err := budget.Check(clustersOnProvider(owned, provider), c); err != nil {
		return nil, fmt.Errorf("%s: %w", provider, err)
	}

	// The owner's quota is one conserved pool spanning every infrastructure, so it cannot on its
	// own keep a single backend from being oversubscribed: a tenant granted the whole platform
	// pool could spend all of it on the KVM host. Each infrastructure's own ceiling is the check
	// that prevents that, and it applies to every provider.
	onProvider := clustersOnProvider(allClusters, c.InfraProvider())
	if err := a.checkProviderCapacity(c.InfraProvider(), onProvider, c); err != nil {
		return nil, err
	}

	// Node networking is the provider-shaped part of admission.
	switch s, shared := a.sharedNet[c.InfraProvider()]; {
	case shared:
		// vSphere and Proxmox clusters all share the operator's network (a portgroup / a bridge):
		// no per-cluster CIDR to allocate, but node/VIP addressing on that shared network must be
		// resolved here (see admitSharedNetwork).
		if strings.TrimSpace(req.NetworkCIDR) != "" {
			return nil, fmt.Errorf("network_cidr cannot be set on %s - the node network is deployment configuration (%s)", c.InfraProvider(), s.NetCIDR)
		}
		if err := a.admitSharedNetwork(c, s, req.APIVIP, req.LoadBalancerIP, onProvider); err != nil {
			return nil, err
		}
	default: // kvm
		// Each cluster gets its own dedicated, isolated node network. Auto-allocate a free block
		// from the platform supernet, or validate the caller's requested CIDR against reserved
		// ranges and every live KVM cluster (see internal/netpool) - a first-class admission check
		// like quota. vSphere clusters are excluded: they all sit on the same operator subnet,
		// which is no more "taken" than the office LAN is.
		networkCIDR, err := netpool.Allocate(clustersOnProvider(allClusters, domain.ProviderKVM), req.NetworkCIDR)
		if err != nil {
			return nil, err
		}
		c.NetworkCIDR = networkCIDR

		// An HA control plane needs a stable endpoint in front of the API servers. With a dedicated
		// per-cluster subnet the VIP is a fixed high host in it (no cross-cluster collision possible),
		// so it's derived deterministically rather than pooled. Demo IPAM - production would use a real
		// allocator (MetalLB-style) or an external load balancer.
		if req.HA {
			c.APIVIP, err = netpool.VIP(networkCIDR)
			if err != nil {
				return nil, err
			}
		}

		// Every cluster reserves one address for its default MetalLB pool / Envoy Gateway (the
		// metallb + envoy-gateway add-ons ship by default). With a dedicated per-cluster subnet it's a
		// fixed high host, derived deterministically like the VIP - no pool needed. If the user
		// deselected those add-ons the reservation is harmless (the wiring gates on them being
		// installed); production would use a real pooled allocator rather than a single derived IP.
		c.LoadBalancerIP, err = netpool.LoadBalancerIP(networkCIDR)
		if err != nil {
			return nil, err
		}
	}

	// The cluster's DNS namespace, derived once here and then stored: "<name>.kaas.example.internal"
	// and the apps domain under it whose wildcard the platform publishes onto LoadBalancerIP (see
	// internal/dns and reconcileDNSWiring). No allocator and no locking are needed - cluster names are
	// globally unique (the clusters.name constraint), which is what makes the domain unique too.
	c.DNSDomain, c.AppsDomain, err = a.dns.AdmitCluster(c.Name)
	if err != nil {
		return nil, err
	}

	if err := a.Store.CreateCluster(c); err != nil {
		return nil, err
	}
	return c, nil
}

// recordOp appends an entry to the cluster's action history in the "in_progress" state, attributed
// to actor; the reconciler flips it to "completed" once the cluster converges to `generation`.
// Best-effort: a recording failure is logged but never fails the user action that triggered it.
func (a *App) recordOp(actor *domain.User, clusterID string, kind domain.OperationKind, generation int64, summary, detail string) {
	op := &domain.Operation{
		ID:         newID(),
		ClusterID:  clusterID,
		Kind:       kind,
		Summary:    summary,
		Detail:     detail,
		Generation: generation,
		Status:     domain.OpInProgress,
		StartedAt:  time.Now(),
	}
	if actor != nil {
		op.ActorID = actor.ID
		op.ActorUsername = actor.Username
	}
	if err := a.Store.RecordOperation(op); err != nil {
		a.Log.Warn("record operation", "cluster", clusterID, "kind", kind, "err", err)
	}
}

// upgradeDetail renders the per-component diff of a bundle promotion (Kubernetes / OS / CNI /
// add-ons) as a single line, for the operation's detail. Best-effort: empty if either bundle
// can't be resolved.
func (a *App) upgradeDetail(fromBundle, toBundle string) string {
	from, err1 := a.Catalog.Resolve(fromBundle)
	to, err2 := a.Catalog.Resolve(toBundle)
	if err1 != nil || err2 != nil {
		return ""
	}
	d := catalog.DiffResolved(from, to)
	var parts []string
	if d.K8sChanged {
		parts = append(parts, fmt.Sprintf("Kubernetes %s → %s", from.Kubernetes, to.Kubernetes))
	}
	if d.OSChanged {
		parts = append(parts, fmt.Sprintf("OS %s → %s", from.OS.Name, to.OS.Name))
	}
	if d.CNIChanged {
		parts = append(parts, fmt.Sprintf("CNI %s %s → %s %s", from.CNI.Name, from.CNI.Version, to.CNI.Name, to.CNI.Version))
	}
	for _, ac := range d.AddonChanges {
		if ac.From == "" {
			parts = append(parts, fmt.Sprintf("add-on %s %s", ac.Name, ac.To))
		} else {
			parts = append(parts, fmt.Sprintf("add-on %s %s → %s", ac.Name, ac.From, ac.To))
		}
	}
	return strings.Join(parts, "; ")
}

// resolveAddons picks the add-ons to install with versions pinned by the bundle. With NO selection
// at all (a nil list - the field omitted), it uses the bundle's add-ons; otherwise it validates each
// requested add-on and pins its version from the bundle (falling back to the catalog's current one).
// nil and empty are deliberately different: with the lock lifted, "I want none of them" is a real
// answer and an empty list is how it is spelled.
//
// Unless the deployment sets KAAS_BUNDLE_ADDONS_OPTIONAL, an explicit selection may only ADD to the
// bundle's own add-ons, never drop one: the batteries-included set is what a cluster is born with,
// and dropping one is an edit made on a Ready cluster. This is the API-side half of the wizard's
// locked add-on cards - the portal renders the lock, but here is where it is enforced.
func (a *App) resolveAddons(rb catalog.ResolvedBundle, requested []string, overrides map[string]string) ([]domain.Addon, error) {
	if requested != nil && !a.BundleAddonsOptional {
		sel := make(map[string]bool, len(requested))
		for _, n := range requested {
			sel[n] = true
		}
		// rb.Addons is the bundle minus the CNI, which is never selectable - it is installed at
		// bootstrap, not as an add-on.
		var missing []string
		for _, ad := range rb.Addons {
			if !sel[ad.Name] {
				missing = append(missing, ad.Name)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("add-ons %s ship with bundle %s and cannot be deselected at create time (they can be removed once the cluster is Ready; a deployment may set KAAS_BUNDLE_ADDONS_OPTIONAL=true to allow it here)", strings.Join(missing, ", "), rb.Name)
		}
	}
	var out []domain.Addon
	if requested == nil {
		out = make([]domain.Addon, 0, len(rb.Addons))
		for _, ad := range rb.Addons {
			out = append(out, domain.Addon{Name: ad.Name, Version: ad.Version, Phase: "pending"})
		}
	} else {
		bundle, _ := a.Catalog.Bundle(rb.Name)
		out = make([]domain.Addon, 0, len(requested))
		for _, name := range requested {
			ver, ok := bundle.Addons[name]
			if !ok {
				ca, known := a.Catalog.Addon(name)
				if !known {
					return nil, fmt.Errorf("unknown add-on %q", name)
				}
				ver = ca.Version
			}
			out = append(out, domain.Addon{Name: name, Version: ver, Phase: "pending"})
		}
	}
	// Apply any per-add-on values overrides from the wizard's editor (validated as YAML).
	for i := range out {
		if ov, ok := overrides[out[i].Name]; ok && strings.TrimSpace(ov) != "" {
			if err := values.Valid(ov); err != nil {
				return nil, fmt.Errorf("add-on %q values: %w", out[i].Name, err)
			}
			out[i].ValuesOverride = ov
		}
	}
	// The reconciler installs add-ons in c.Addons order, so pin the install order here. The bundle's
	// own platform add-ons install FIRST, ahead of any optional add-on the user picked from the
	// catalog - kube-prometheus-stack (a bundled add-on) brings up the ServiceMonitor/Prometheus CRDs
	// that many optional add-ons publish into, so those CRDs must exist before an optional add-on's
	// Helm install references them. Within each group, (Priority, Name) keeps a deterministic order
	// (and keeps kube-prometheus-stack's -100 ahead of the other platform add-ons). Same (Priority,
	// Name) tiebreak catalog.Resolve applies to a bundle's default set.
	bundle, _ := a.Catalog.Bundle(rb.Name)
	isBundled := func(name string) bool { _, ok := bundle.Addons[name]; return ok }
	sort.SliceStable(out, func(i, j int) bool {
		if bi, bj := isBundled(out[i].Name), isBundled(out[j].Name); bi != bj {
			return bi // bundled platform add-ons before optional ones
		}
		pi, pj := a.addonPriority(out[i].Name), a.addonPriority(out[j].Name)
		if pi != pj {
			return pi < pj
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// addonPriority is the catalog install-order priority for an add-on (0 for an unknown one).
func (a *App) addonPriority(name string) int {
	if ca, ok := a.Catalog.Addon(name); ok {
		return ca.Priority
	}
	return 0
}

// Operations returns the cluster's action history (create, scale, add-on, upgrade), newest first.
func (a *App) Operations(actor *domain.User, id string) ([]*domain.Operation, error) {
	if _, err := a.authorizeCluster(actor, id); err != nil {
		return nil, err
	}
	return a.Store.ListOperations(id)
}

// Upgrades returns the release bundles this cluster can be promoted to.
func (a *App) Upgrades(actor *domain.User, id string) ([]catalog.Bundle, error) {
	c, err := a.authorizeCluster(actor, id)
	if err != nil {
		return nil, err
	}
	return a.Catalog.UpgradesFor(c.Bundle), nil
}

// PromoteCluster records a desired upgrade to targetBundle (writing TargetBundle and bumping the
// generation); the reconciler advances the running cluster one supersedes hop at a time toward it.
// It validates the target is reachable on the cluster's upgrade chain. An OS change is handled per
// topology by the reconciler - rolling replacement for HA, backup/restore for a single control
// plane - so no topology is rejected here. Replacement is in-place (node count unchanged), so no
// extra quota is charged.
func (a *App) PromoteCluster(actor *domain.User, id, targetBundle string) (*domain.Cluster, error) {
	c, err := a.authorizeClusterWrite(actor, id)
	if err != nil {
		return nil, err
	}
	if c.Phase != domain.PhaseReady {
		return nil, fmt.Errorf("cluster %q is not Ready (phase %s) - wait before upgrading", c.Name, c.Phase)
	}
	if targetBundle == "" {
		return nil, fmt.Errorf("target bundle is required")
	}
	if targetBundle == c.Bundle {
		return nil, fmt.Errorf("cluster %q already runs bundle %q", c.Name, c.Bundle)
	}
	reachable := false
	for _, b := range a.Catalog.UpgradesFor(c.Bundle) {
		if b.Name == targetBundle {
			reachable = true
			break
		}
	}
	if !reachable {
		return nil, fmt.Errorf("bundle %q is not an available upgrade from %q", targetBundle, c.Bundle)
	}
	// Purge any control-plane backup left by an earlier (possibly crashed) upgrade of this cluster,
	// so a single-node OS roll takes a FRESH etcd snapshot rather than silently reusing a stale one
	// (which would roll etcd back and drop nodes that joined since). Safe here: the cluster is Ready,
	// so no roll is in flight. Best-effort - a failure here must not block the promotion.
	if err := a.Rec.Cfg.DiscardControlPlaneBackup(context.Background(), c); err != nil {
		a.Log.Warn("discard stale control-plane backup", "cluster", c.Name, "err", err)
	}

	fromBundle := c.Bundle
	c.TargetBundle = targetBundle
	c.Generation++
	if err := a.Store.UpdateCluster(c); err != nil {
		return nil, err
	}
	a.recordOp(actor, c.ID, domain.OpUpgrade, c.Generation,
		fmt.Sprintf("Upgrade %s → %s", fromBundle, targetBundle), a.upgradeDetail(fromBundle, targetBundle))
	return c, nil
}

// UpdateRequest is an in-place edit to a live cluster. Nil fields are left unchanged,
// so a caller can scale workers, change the add-on set, or both.
type UpdateRequest struct {
	// NodePools is the desired worker topology: the WHOLE pool list, replacing what's there (the same
	// declarative shape as Addons below). nil leaves the pools untouched. Scaling a pool, adding one
	// and removing one are all the same edit - send the list you want.
	//
	// A pool's Size is immutable (see domain.NodePool): an edit that changes the size of an existing
	// pool is rejected rather than silently rolling every node in it.
	NodePools *[]domain.NodePool `json:"node_pools,omitempty"`
	Addons    *[]string          `json:"addons,omitempty"`
	// AddonValues carries starting Helm values overrides (add-on name -> edited YAML) for add-ons
	// being ADDED in this edit. They land only on newly-appended add-ons; an add-on already on the
	// cluster keeps its stored override (change it via the per-add-on values endpoint instead).
	AddonValues map[string]string `json:"addon_values,omitempty"`
	// CustomAddons is the desired set of custom-catalog add-ons (see internal/domain.CustomCatalog).
	// nil leaves existing custom add-ons untouched (so a workers/built-in-only edit never disturbs
	// them); non-nil reconciles the custom partition of the add-on list toward it.
	CustomAddons *[]domain.CustomAddonRef `json:"custom_addons,omitempty"`
}

// UpdateCluster applies an edit (scale workers up/down, add/remove add-ons), enforces the
// capacity budget, and bumps the generation. The reconciler notices generation !=
// observed_generation and converges the running cluster to the new desired state.
func (a *App) UpdateCluster(actor *domain.User, id string, req UpdateRequest) (*domain.Cluster, error) {
	if _, err := a.authorizeClusterWrite(actor, id); err != nil {
		return nil, err
	}
	// Scaling workers is a quota decision, so - exactly like CreateCluster - it must not interleave
	// with another replica's admission. Hold the lock and re-read the cluster inside it: the copy
	// authorizeClusterWrite returned was read before the lock and may already be stale.
	var c *domain.Cluster
	var ops []pendingOp
	if err := a.Store.WithLock(store.LockAdmission, func() error {
		fresh, err := a.Store.GetCluster(id)
		if err != nil {
			return err
		}
		if fresh.Phase == domain.PhaseDeleting || fresh.Phase.Terminal() {
			return fmt.Errorf("cluster %q is not editable (phase %s)", fresh.Name, fresh.Phase)
		}
		ops, err = a.applyUpdate(actor, fresh, req)
		if err != nil {
			return err
		}
		if len(ops) > 0 {
			fresh.Generation++
			if err := a.Store.UpdateCluster(fresh); err != nil {
				return err
			}
		}
		c = fresh
		return nil
	}); err != nil {
		return nil, err
	}
	for _, op := range ops {
		a.recordOp(actor, c.ID, op.kind, c.Generation, op.summary, op.detail)
	}
	return c, nil
}

// applyUpdate mutates c toward req (scale workers, add/remove add-ons), enforcing the owner's
// capacity budget, and returns the operations to record - empty when the edit is a no-op. It does
// not persist: UpdateCluster does that, under store.LockAdmission, which callers MUST hold since
// the quota check here reads the owner's other clusters.
func (a *App) applyUpdate(actor *domain.User, c *domain.Cluster, req UpdateRequest) ([]pendingOp, error) {
	// Operations to record once the edit is persisted (with the bumped generation). Summaries are
	// captured here, before the mutations below overwrite the values they compare against.
	var ops []pendingOp

	if req.NodePools != nil {
		poolOps, err := a.applyNodePools(c, *req.NodePools)
		if err != nil {
			return nil, err
		}
		ops = append(ops, poolOps...)
	}

	if req.Addons != nil {
		// summary computed before applyAddonSelection mutates c.Addons; built-in partition only.
		summary := addonSelectionSummary(c, *req.Addons, isBuiltinAddon)
		addonChanged, err := a.applyAddonSelection(c, *req.Addons, req.AddonValues)
		if err != nil {
			return nil, err
		}
		if addonChanged {
			ops = append(ops, pendingOp{domain.OpAddons, summary, ""})
		}
	}

	if req.CustomAddons != nil {
		names := make([]string, 0, len(*req.CustomAddons))
		for _, ref := range *req.CustomAddons {
			names = append(names, ref.Name)
		}
		summary := addonSelectionSummary(c, names, domain.Addon.Custom) // custom partition only
		addonChanged, err := a.applyCustomAddonSelection(actor, c, *req.CustomAddons)
		if err != nil {
			return nil, err
		}
		if addonChanged {
			ops = append(ops, pendingOp{domain.OpAddons, summary, ""})
		}
	}

	return ops, nil
}

// applyNodePools converges c's worker topology onto the desired pool list, enforcing the owner's
// capacity budget and the infrastructure's ceiling, and returns the operations to record. A no-op
// edit (the list already matches) returns none, so the generation isn't bumped for nothing.
//
// The checks price a CANDIDATE - a copy of c carrying the desired pools - because a cluster's cost
// is now a function of its whole shape (control-plane size + every pool's size), not of a worker
// count. c is only mutated once every check has passed, so a rejected edit leaves it untouched.
// Callers MUST hold store.LockAdmission (see UpdateCluster): every read here is a
// read-then-write against the live cluster set.
func (a *App) applyNodePools(c *domain.Cluster, want []domain.NodePool) ([]pendingOp, error) {
	if err := domain.ValidateNodePools(c.Name, want); err != nil {
		return nil, err
	}
	// A pool's size is fixed for its lifetime: changing it would mean rolling every node in the pool
	// (or resizing live VMs under a running kubelet). Reject it explicitly rather than let the
	// reconciler act on a shape the user didn't realise they were asking for - the supported path is
	// a new pool at the new size, draining the old one away.
	for _, w := range want {
		cur, ok := c.Pool(w.Name)
		if !ok {
			continue
		}
		if cur.Size != w.Size {
			return nil, fmt.Errorf("node pool %q: size is immutable (%s); add a pool at the new size and remove this one",
				w.Name, cur.Size)
		}
		// The root-disk override is immutable for the same reason, and a sharper one: growing it
		// re-creates each node's libvirt volume, which (via the module's replace_triggered_by)
		// destroys and rebuilds every VM in the pool underneath a live kubelet. A user who wants
		// more storage on a RUNNING node wants an extra disk (domain.NodeDisk) - which is
		// non-destructive - or a new pool at the bigger root disk.
		if cur.DiskGB != w.DiskGB {
			return nil, fmt.Errorf("node pool %q: disk size is immutable (%d GB); add an extra disk to its nodes, or add a pool at the new disk size and remove this one",
				w.Name, cur.RootDiskGB())
		}
	}
	ops := nodePoolOps(c.NodePools, want)
	if len(ops) == 0 {
		return nil, nil
	}

	candidate := *c
	candidate.NodePools = want
	// Scaling a pool down - or deleting it outright - un-desires its highest-numbered VM names, and
	// any extra disk pinned to one goes with it: the node is being destroyed, so its disks are too
	// (the provisioner drops the volumes along with the VM, and there is nothing to unmount from a
	// machine that no longer exists).
	//
	// Pruning here rather than leaving the rows is not tidiness. A disk naming a node that
	// DesiredNodes no longer mints is desired state nothing converges - and ValidateNodeDisks
	// rejects exactly that, so a stale row would make every later disk edit on this cluster fail.
	// Doing it on the candidate also means the quota check below prices the disks the user will
	// actually still have.
	candidate.NodeDisks = disksOnDesiredNodes(&candidate)
	// ...and give any node the edit ADDS its platform storage disk, so a scaled-up pool's new workers
	// come up with the same Longhorn capacity as their siblings. Both halves run before the quota
	// checks below, which therefore price the disk set the user will actually end up with.
	candidate.NodeDisks = syncStorageDisks(&candidate)

	// Quota is charged to the cluster's OWNER (correct even when an admin scales someone else's
	// cluster), against the owner's grant on THIS cluster's infrastructure and their other clusters
	// on it.
	ownerBudget, ownerClusters, err := a.ownerBudget(c.OwnerID, c.InfraProvider())
	if err != nil {
		return nil, err
	}
	if err := ownerBudget.Check(clustersExcept(ownerClusters, c.ID), &candidate); err != nil {
		return nil, fmt.Errorf("%s: %w", c.InfraProvider(), err)
	}
	// The infrastructure's own platform-wide ceiling applies to scaling too, and a static-mode
	// shared-network cluster's per-node IP allocation must follow the new node set (both need the
	// admission lock UpdateCluster holds - see admitCluster).
	all, err := a.Store.ListClusters()
	if err != nil {
		return nil, err
	}
	onProvider := clustersOnProvider(all, c.InfraProvider())
	if err := a.checkProviderCapacity(c.InfraProvider(), clustersExcept(onProvider, c.ID), &candidate); err != nil {
		return nil, err
	}
	if err := a.scaleSharedStaticIPs(&candidate); err != nil {
		return nil, err
	}

	c.NodePools = want
	c.StaticIPs = candidate.StaticIPs
	c.NodeDisks = candidate.NodeDisks
	return ops, nil
}

// disksOnDesiredNodes drops any extra disk pinned to a VM name the cluster no longer desires - the
// nodes a pool scale-down or deletion is taking away. Order is preserved.
func disksOnDesiredNodes(c *domain.Cluster) []domain.NodeDisk {
	desired := make(map[string]bool)
	for _, d := range domain.DesiredNodes(c) {
		desired[d.VMName] = true
	}
	kept := make([]domain.NodeDisk, 0, len(c.NodeDisks))
	for _, d := range c.NodeDisks {
		if desired[d.VMName] {
			kept = append(kept, d)
		}
	}
	return kept
}

// nodePoolOps diffs the current pool list against the desired one and describes each change for the
// action history: pools added, pools removed, and pools whose worker count moved. Empty when the two
// lists describe the same topology.
func nodePoolOps(current, want []domain.NodePool) []pendingOp {
	var ops []pendingOp
	have := make(map[string]domain.NodePool, len(current))
	for _, p := range current {
		have[p.Name] = p
	}
	wanted := make(map[string]bool, len(want))
	for _, w := range want {
		wanted[w.Name] = true
		cur, existed := have[w.Name]
		switch {
		case !existed:
			ops = append(ops, pendingOp{domain.OpScale,
				fmt.Sprintf("Added node pool %q - %d × %s", w.Name, w.DesiredWorkers, w.Size), ""})
		case cur.DesiredWorkers != w.DesiredWorkers:
			verb := "Scaled up"
			if w.DesiredWorkers < cur.DesiredWorkers {
				verb = "Scaled down"
			}
			ops = append(ops, pendingOp{domain.OpScale,
				fmt.Sprintf("%s node pool %q %d → %d workers", verb, w.Name, cur.DesiredWorkers, w.DesiredWorkers), ""})
		}
	}
	for _, p := range current {
		if !wanted[p.Name] {
			ops = append(ops, pendingOp{domain.OpScale,
				fmt.Sprintf("Removed node pool %q - %d × %s drained", p.Name, p.DesiredWorkers, p.Size), ""})
		}
	}
	return ops
}

// pendingOp is an operation captured mid-edit, recorded after the edit persists (see UpdateCluster).
type pendingOp struct {
	kind    domain.OperationKind
	summary string
	detail  string
}

// isBuiltinAddon selects the built-in (non-custom) partition of a cluster's add-ons.
func isBuiltinAddon(ad domain.Addon) bool { return !ad.Custom() }

// addonSelectionSummary renders the add-on delta ("+prometheus, -ingress-nginx") between the
// cluster's currently-installed set and the requested set, in a stable order, scoped to the add-ons
// `governs` selects (built-in or custom partition). Must be called before the selection mutates
// c.Addons.
func addonSelectionSummary(c *domain.Cluster, requested []string, governs func(domain.Addon) bool) string {
	want := make(map[string]bool, len(requested))
	for _, name := range requested {
		want[name] = true
	}
	have := make(map[string]bool, len(c.Addons))
	var parts []string
	for _, ad := range c.Addons {
		if ad.Phase == "removing" || !governs(ad) {
			continue
		}
		have[ad.Name] = true
		if !want[ad.Name] {
			parts = append(parts, "-"+ad.Name)
		}
	}
	for _, name := range requested { // additions in request order
		if !have[name] {
			parts = append(parts, "+"+name)
		}
	}
	if len(parts) == 0 {
		return "Add-ons updated"
	}
	return "Add-ons: " + strings.Join(parts, ", ")
}

// applyAddonSelection reconciles the BUILT-IN partition of the cluster's add-on list toward the
// requested set of names (versions pinned by the cluster's bundle, falling back to the catalog's
// current). Custom add-ons are left untouched (they are reconciled by applyCustomAddonSelection).
// Newly-requested add-ons are appended "pending"; dropped ones are marked "removing"; the reconciler
// performs the Helm install/uninstall and then drops removed entries. overrides supplies optional
// starting Helm values for newly-added add-ons - reconcileAddonSet keeps an existing add-on's stored
// record, so these land only on additions (an existing add-on's override is edited via its own
// endpoint).
func (a *App) applyAddonSelection(c *domain.Cluster, requested []string, overrides map[string]string) (bool, error) {
	bundle, hasBundle := a.Catalog.Bundle(c.Bundle)
	want := make(map[string]domain.Addon, len(requested))
	for _, name := range requested {
		ver := ""
		if hasBundle {
			if v, ok := bundle.Addons[name]; ok {
				ver = v
			}
		}
		if ver == "" {
			ca, known := a.Catalog.Addon(name)
			if !known {
				return false, fmt.Errorf("unknown add-on %q", name)
			}
			ver = ca.Version
		}
		ad := domain.Addon{Name: name, Version: ver, Phase: "pending"}
		if ov, ok := overrides[name]; ok && strings.TrimSpace(ov) != "" {
			if err := values.Valid(ov); err != nil {
				return false, fmt.Errorf("add-on %q values: %w", name, err)
			}
			ad.ValuesOverride = ov
		}
		want[name] = ad
	}
	return reconcileAddonSet(c, want, requested, isBuiltinAddon), nil
}

// applyCustomAddonSelection reconciles the CUSTOM partition of the cluster's add-on list toward the
// requested (catalog, add-on) refs, leaving built-in add-ons untouched. Each ref is resolved to a
// self-contained add-on (chart definition copied from the catalog); a re-selected custom add-on
// already on the cluster keeps its stored record (values included).
func (a *App) applyCustomAddonSelection(actor *domain.User, c *domain.Cluster, refs []domain.CustomAddonRef) (bool, error) {
	// Guard against a custom add-on name colliding with a built-in add-on staying on the cluster.
	taken := make(map[string]bool, len(c.Addons))
	for _, ad := range c.Addons {
		if !ad.Custom() {
			taken[ad.Name] = true
		}
	}
	resolved, err := a.resolveCustomAddons(actor, refs, taken)
	if err != nil {
		return false, err
	}
	want := make(map[string]domain.Addon, len(resolved))
	order := make([]string, 0, len(resolved))
	for _, ad := range resolved {
		want[ad.Name] = ad
		order = append(order, ad.Name)
	}
	return reconcileAddonSet(c, want, order, domain.Addon.Custom), nil
}

// reconcileAddonSet advances the add-ons that `governs` selects toward `want` (keyed by name),
// leaving every other add-on untouched. order gives the deterministic append order for additions.
// A governed add-on in `want` is kept (a pending removal is cancelled); one not in `want` is marked
// "removing"; a wanted name not yet present is appended from `want`. Returns whether anything changed.
func reconcileAddonSet(c *domain.Cluster, want map[string]domain.Addon, order []string, governs func(domain.Addon) bool) bool {
	changed := false
	next := make([]domain.Addon, 0, len(c.Addons)+len(want))
	seen := make(map[string]bool, len(c.Addons))
	for _, ad := range c.Addons {
		if !governs(ad) { // a different partition - pass through unchanged
			next = append(next, ad)
			continue
		}
		if _, keep := want[ad.Name]; keep {
			seen[ad.Name] = true
			if ad.Phase == "removing" { // re-adding cancels a pending removal
				ad.Phase = "pending"
				changed = true
			}
			next = append(next, ad)
			continue
		}
		if ad.Phase != "removing" { // schedule removal (once)
			ad.Phase = "removing"
			changed = true
		}
		next = append(next, ad)
	}
	for _, name := range order { // additions, in request order
		if !seen[name] {
			next = append(next, want[name])
			seen[name] = true
			changed = true
		}
	}
	c.Addons = next
	return changed
}

// AddonValuesView is the payload behind the in-browser values editor: the chart's own defaults, the
// platform's curated catalog overrides, and the two merged (the editor's seed for an un-customized
// install). Override/Phase are populated only for a cluster-scoped view (an existing add-on's saved
// override and its current lifecycle phase).
type AddonValuesView struct {
	Addon            string            `json:"addon"`
	Version          string            `json:"version"`
	ChartValues      string            `json:"chart_values"`       // the chart's own values.yaml (provider)
	CatalogOverrides map[string]string `json:"catalog_overrides"`  // the platform-curated --set overrides
	EffectiveValues  string            `json:"effective_values"`   // chart defaults + catalog overrides, merged
	Override         string            `json:"override,omitempty"` // per-cluster saved override, if any
	Phase            string            `json:"phase,omitempty"`    // add-on lifecycle phase (cluster-scoped)
}

// addonValuesView resolves a catalog add-on's editor payload: fetch the chart defaults via the
// values provider and merge the catalog overrides. version pins which chart the provider fetches.
func (a *App) addonValuesView(ctx context.Context, entry catalog.Addon, version string) (*AddonValuesView, error) {
	if version != "" {
		entry.Version = version
	}
	chart, err := a.Values.Defaults(ctx, entry)
	if err != nil {
		return nil, err
	}
	effective, err := values.Merged(entry, chart)
	if err != nil {
		return nil, err
	}
	return &AddonValuesView{
		Addon:            entry.Name,
		Version:          entry.Version,
		ChartValues:      chart,
		CatalogOverrides: entry.Values,
		EffectiveValues:  effective,
	}, nil
}

// AddonValues returns the editor payload for a catalog add-on during cluster creation (no cluster
// yet). The version is pinned by the chosen bundle when it lists the add-on, else the catalog's.
func (a *App) AddonValues(ctx context.Context, actor *domain.User, bundleName, addonName string) (*AddonValuesView, error) {
	if actor == nil {
		return nil, ErrForbidden
	}
	entry, ok := a.Catalog.Addon(addonName)
	if !ok {
		return nil, fmt.Errorf("unknown add-on %q", addonName)
	}
	version := entry.Version
	if b, ok := a.Catalog.Bundle(bundleName); ok {
		if v, ok := b.Addons[addonName]; ok {
			version = v
		}
	}
	return a.addonValuesView(ctx, entry, version)
}

// ClusterAddonValues returns the editor payload for one of a cluster's add-ons, plus that cluster's
// saved override (if any) and the add-on's current phase, so the editor can seed from the override
// and show whether it is customized. Read access suffices to view.
func (a *App) ClusterAddonValues(ctx context.Context, actor *domain.User, id, addonName string) (*AddonValuesView, error) {
	c, err := a.authorizeCluster(actor, id)
	if err != nil {
		return nil, err
	}
	var installed *domain.Addon
	for i := range c.Addons {
		if c.Addons[i].Name == addonName {
			installed = &c.Addons[i]
			break
		}
	}
	if installed == nil {
		return nil, fmt.Errorf("add-on %q is not on cluster %q", addonName, c.Name)
	}
	// Built-in add-ons resolve their chart from the catalog; a custom add-on carries its own chart
	// definition on the cluster record (self-contained), so the editor payload comes from there.
	entry, ok := a.Catalog.Addon(addonName)
	if !ok {
		if !installed.Custom() {
			return nil, fmt.Errorf("unknown add-on %q", addonName)
		}
		entry = catalog.Addon{Name: installed.Name, Chart: installed.Chart, Repo: installed.Repo, Namespace: installed.Namespace}
	}
	view, err := a.addonValuesView(ctx, entry, installed.Version)
	if err != nil {
		return nil, err
	}
	view.Override = installed.ValuesOverride
	view.Phase = installed.Phase
	return view, nil
}

// SetClusterAddonValues records a per-cluster Helm values override for an add-on (write-scoped). It
// validates the YAML, flips the add-on to "updating", bumps the generation, and records an operation
// - the reconciler then runs `helm upgrade` with the new values. An empty override resets the add-on
// to the curated catalog defaults (also an update). The add-on must already be on the cluster.
func (a *App) SetClusterAddonValues(actor *domain.User, id, addonName, override string) (*domain.Cluster, error) {
	c, err := a.authorizeClusterWrite(actor, id)
	if err != nil {
		return nil, err
	}
	if c.Phase != domain.PhaseReady {
		return nil, fmt.Errorf("cluster %q is not Ready (phase %s) - wait before editing add-on values", c.Name, c.Phase)
	}
	if err := values.Valid(override); err != nil {
		return nil, fmt.Errorf("add-on %q values: %w", addonName, err)
	}
	var target *domain.Addon
	for i := range c.Addons {
		if c.Addons[i].Name == addonName && c.Addons[i].Phase != "removing" {
			target = &c.Addons[i]
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("add-on %q is not installed on cluster %q", addonName, c.Name)
	}
	if strings.TrimSpace(target.ValuesOverride) == strings.TrimSpace(override) {
		return c, nil // no change
	}
	target.ValuesOverride = override
	target.Phase = "updating"
	c.Generation++
	if err := a.Store.UpdateCluster(c); err != nil {
		return nil, err
	}
	summary := fmt.Sprintf("Edited %q add-on values", addonName)
	if strings.TrimSpace(override) == "" {
		summary = fmt.Sprintf("Reset %q add-on values to defaults", addonName)
	}
	a.recordOp(actor, c.ID, domain.OpAddons, c.Generation, summary, "")
	return c, nil
}

// Capacity reports the actor's quota vs. their current allocation ON EACH INFRASTRUCTURE, so the
// UI can show a tenant their headroom where it actually exists. The top-level totals are the sum
// across providers - a fleet-wide summary, NOT something a single cluster can be admitted against
// (each provider's own entry is what admission charges). (The platform-wide allocation is on the
// admin's ListUsers view.)
func (a *App) Capacity(actor *domain.User) (CapacityReport, error) {
	if actor == nil {
		return CapacityReport{}, ErrForbidden
	}
	owned, err := a.Store.ListClustersByOwner(actor.ID)
	if err != nil {
		return CapacityReport{}, err
	}
	rep := CapacityReport{}
	for _, p := range a.infraProviders() {
		budget, err := a.budgetFor(actor, p)
		if err != nil {
			return CapacityReport{}, err
		}
		usedCPU, usedMem, usedDisk := quota.Usage(clustersOnProvider(owned, p))
		rep.Providers = append(rep.Providers, ProviderQuota{
			Provider:    p,
			TotalVCPU:   budget.TotalVCPU,
			TotalMemMB:  budget.TotalMemMB,
			TotalDiskGB: budget.TotalDiskGB,
			UsedVCPU:    usedCPU,
			UsedMemMB:   usedMem,
			UsedDiskGB:  usedDisk,
		})
		rep.TotalVCPU += budget.TotalVCPU
		rep.TotalMemMB += budget.TotalMemMB
		rep.TotalDiskGB += budget.TotalDiskGB
		rep.UsedVCPU += usedCPU
		rep.UsedMemMB += usedMem
		rep.UsedDiskGB += usedDisk
	}
	return rep, nil
}

// ProviderQuota is one infrastructure's slice of an account's quota: what they were granted there,
// and what they're using there.
type ProviderQuota struct {
	Provider    string `json:"provider"`
	TotalVCPU   int    `json:"total_vcpu"`
	TotalMemMB  int    `json:"total_mem_mb"`
	TotalDiskGB int    `json:"total_disk_gb"`
	UsedVCPU    int    `json:"used_vcpu"`
	UsedMemMB   int    `json:"used_mem_mb"`
	UsedDiskGB  int    `json:"used_disk_gb"`
}

// CapacityReport is the actor's quota snapshot returned by GET /capacity. Providers is the real
// unit: one entry per infrastructure, holding the grant and the usage there. The top-level Total*
// / Used* are their sum - a fleet-wide headline only, since capacity doesn't move between
// backends and no single cluster is ever admitted against the sum.
type CapacityReport struct {
	TotalVCPU   int             `json:"total_vcpu"`
	TotalMemMB  int             `json:"total_mem_mb"`
	TotalDiskGB int             `json:"total_disk_gb"`
	UsedVCPU    int             `json:"used_vcpu"`
	UsedMemMB   int             `json:"used_mem_mb"`
	UsedDiskGB  int             `json:"used_disk_gb"`
	Providers   []ProviderQuota `json:"providers"`
}

// Profile is the actor's own account view (GET /auth/profile): who they are, the groups they belong
// to with the role they hold in each, and their per-infrastructure quota and usage.
//
// It exists because a user cannot answer "which groups am I in?" from /auth/me alone: memberships
// carry only a group ID, and ListGroups - the only thing that resolves IDs to names - is admin-only.
// Rather than widen that, this resolves the actor's OWN memberships and nothing else, so it stays
// safe for any authenticated caller. The quota half is Capacity verbatim.
func (a *App) Profile(actor *domain.User) (*ProfileReport, error) {
	if actor == nil {
		return nil, ErrForbidden
	}
	cap, err := a.Capacity(actor)
	if err != nil {
		return nil, err
	}
	rep := &ProfileReport{User: actor, Capacity: cap}
	for _, m := range actor.Memberships {
		g, err := a.Store.GetGroup(m.GroupID)
		if err != nil {
			// A membership naming a group that's gone is a torn read against a concurrent group
			// delete, not a reason to fail the page - skip it and let the next load settle.
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, err
		}
		rep.Groups = append(rep.Groups, ProfileGroup{
			ID:     g.ID,
			Name:   g.Name,
			Source: g.Source,
			Role:   m.Role,
		})
	}
	sort.Slice(rep.Groups, func(i, j int) bool { return rep.Groups[i].Name < rep.Groups[j].Name })
	return rep, nil
}

// ProfileGroup is one of the actor's memberships with the group's name resolved, and the role they
// hold in that group (roles are per-membership - Read in one group, Write in another).
type ProfileGroup struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Source string           `json:"source,omitempty"` // local | ldap (see domain.Group.Source)
	Role   domain.GroupRole `json:"role"`
}

// ProfileReport is what GET /auth/profile returns.
type ProfileReport struct {
	User     *domain.User   `json:"user"`
	Groups   []ProfileGroup `json:"groups"`
	Capacity CapacityReport `json:"capacity"`
}

func clustersExcept(list []*domain.Cluster, id string) []*domain.Cluster {
	out := make([]*domain.Cluster, 0, len(list))
	for _, c := range list {
		if c.ID != id {
			out = append(out, c)
		}
	}
	return out
}

// DeleteCluster moves a cluster to Deleting; the reconciler tears it down.
func (a *App) DeleteCluster(actor *domain.User, id string) error {
	c, err := a.authorizeClusterWrite(actor, id)
	if err != nil {
		return err
	}
	if c.Phase == domain.PhaseDeleting || c.Phase == domain.PhaseDeleted {
		return nil
	}
	c.Phase = domain.PhaseDeleting
	c.Generation++
	return a.Store.UpdateCluster(c)
}

// Metrics returns the latest resource-usage snapshot for a cluster (live telemetry sampled by
// the reconciler's metrics ticker). It validates the cluster exists first, so a missing cluster
// is a 404 while a cluster with no snapshot yet (just became Ready, or metrics-server disabled)
// is store.ErrNotFound.
func (a *App) Metrics(actor *domain.User, id string) (*domain.MetricsSnapshot, error) {
	if _, err := a.authorizeCluster(actor, id); err != nil {
		return nil, err
	}
	return a.Store.GetMetrics(id)
}

// Health returns the latest health snapshot for a cluster (live telemetry evaluated by the
// reconciler's health ticker). Like Metrics it validates the cluster exists first, so a missing
// cluster is a 404 while a cluster with no snapshot yet (just became Ready, or health disabled) is
// store.ErrNotFound.
func (a *App) Health(actor *domain.User, id string) (*domain.HealthSnapshot, error) {
	if _, err := a.authorizeCluster(actor, id); err != nil {
		return nil, err
	}
	return a.Store.GetHealth(id)
}

// --- Workloads (request-driven kube query seam; see internal/kube) -----------

// ErrClusterNotReady is returned by the workload and storage methods when a cluster exists and is
// visible to the actor but hasn't reached Ready yet - there is no kubeconfig / reachable API server
// to query. The API maps it to 409 Conflict. Worded for every page that shares it, not just
// Workloads.
var ErrClusterNotReady = errors.New("cluster is not Ready - live cluster data is available once it is Ready")

// workloadRead resolves a cluster for a read-only workloads query: view access (owner, admin, or any
// group-mate), the cluster must be Ready, and returns a per-user kubeconfig carrying the actor's own
// identity and role (a read-role group-mate gets a reader cert → the `view` RBAC; see
// userClusterKubeconfig).
func (a *App) workloadRead(ctx context.Context, actor *domain.User, id string) (*domain.Cluster, []byte, error) {
	c, err := a.authorizeCluster(actor, id)
	if err != nil {
		return nil, nil, err
	}
	if c.Phase != domain.PhaseReady {
		return nil, nil, ErrClusterNotReady
	}
	kc, _, _, err := a.userClusterKubeconfig(ctx, actor, c)
	if err != nil {
		return nil, nil, err
	}
	return c, kc, nil
}

// WorkloadNamespaces lists the cluster's namespaces (for the page's namespace picker).
func (a *App) WorkloadNamespaces(ctx context.Context, actor *domain.User, id string) ([]string, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.Namespaces(ctx, c, kc)
}

// Workloads lists the cluster's workloads (namespace == "" means all namespaces).
func (a *App) Workloads(ctx context.Context, actor *domain.User, id, namespace string) ([]kube.WorkloadSummary, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.Workloads(ctx, c, kc, namespace)
}

// Workload returns one workload's detail (spec rollup + pods).
func (a *App) Workload(ctx context.Context, actor *domain.User, id string, ref kube.WorkloadRef) (*kube.WorkloadDetail, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.Workload(ctx, c, kc, ref)
}

// WorkloadManifest returns a workload's YAML.
func (a *App) WorkloadManifest(ctx context.Context, actor *domain.User, id string, ref kube.WorkloadRef) (string, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return "", err
	}
	return a.Kube.Manifest(ctx, c, kc, ref)
}

// WorkloadEvents returns the events for a workload and its owned objects.
func (a *App) WorkloadEvents(ctx context.Context, actor *domain.User, id string, ref kube.WorkloadRef) ([]kube.Event, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.Events(ctx, c, kc, ref)
}

// ScaleWorkload sets a workload's replica count. It requires write access - a read-role group member
// gets ErrForbidden (403) - and runs as the actor's own per-user (writer) credential, so the mutation
// is attributed to them, not a shared admin identity.
func (a *App) ScaleWorkload(ctx context.Context, actor *domain.User, id string, ref kube.WorkloadRef, replicas int) error {
	c, err := a.authorizeClusterWrite(actor, id)
	if err != nil {
		return err
	}
	if c.Phase != domain.PhaseReady {
		return ErrClusterNotReady
	}
	kc, _, _, err := a.userClusterKubeconfig(ctx, actor, c)
	if err != nil {
		return err
	}
	return a.Kube.Scale(ctx, c, kc, ref, replicas, false)
}

// WorkloadLogs streams a pod's logs to sink until the stream ends or ctx is cancelled. View access
// is enough - a read-role member's viewer RBAC permits reading pod logs.
func (a *App) WorkloadLogs(ctx context.Context, actor *domain.User, id string, ref kube.LogRef, sink kube.LogSink) error {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return err
	}
	return a.Kube.Logs(ctx, c, kc, ref, sink)
}

// --- Storage (request-driven kube query seam; see internal/kube/storage.go) ---
//
// The Storage page reads core Kubernetes objects, so it needs no add-on gate (unlike Monitoring and
// Security) and no admin-kubeconfig shortcut either: every call is view-scoped and read-only, and a
// read-role member's per-user reader credential already covers all of it - PVCs, pods and events via
// the built-in `view` role, PersistentVolumes and StorageClasses via the small cluster-scoped read
// role the kaas:readers group is bound to (see ansible/roles/viewer_kubeconfig). So these all reuse
// workloadRead.

// PVCs lists the cluster's PersistentVolumeClaims (namespace == "" means all namespaces).
func (a *App) PVCs(ctx context.Context, actor *domain.User, id, namespace string) ([]kube.PVCSummary, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.PVCs(ctx, c, kc, namespace)
}

// PVC returns one claim's detail (its bound PV and the pods mounting it).
func (a *App) PVC(ctx context.Context, actor *domain.User, id string, ref kube.PVCRef) (*kube.PVCDetail, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.PVC(ctx, c, kc, ref)
}

// PVCManifest returns a claim's YAML.
func (a *App) PVCManifest(ctx context.Context, actor *domain.User, id string, ref kube.PVCRef) (string, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return "", err
	}
	return a.Kube.PVCManifest(ctx, c, kc, ref)
}

// PVCEvents returns a claim's events (where a provisioning failure shows up).
func (a *App) PVCEvents(ctx context.Context, actor *domain.User, id string, ref kube.PVCRef) ([]kube.Event, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.PVCEvents(ctx, c, kc, ref)
}

// StorageClasses lists the cluster's StorageClasses.
func (a *App) StorageClasses(ctx context.Context, actor *domain.User, id string) ([]kube.StorageClass, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.StorageClasses(ctx, c, kc)
}

// StorageClassManifest returns a StorageClass's YAML (cluster-scoped: name alone identifies one).
func (a *App) StorageClassManifest(ctx context.Context, actor *domain.User, id, name string) (string, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return "", err
	}
	return a.Kube.StorageClassManifest(ctx, c, kc, name)
}

// --- Networking (request-driven kube query seam; see internal/kube/network.go) ---
//
// The Networking page reads Services (core objects) and the Gateway API's Gateways/Routes (CRDs from
// the envoy-gateway add-on). Like Storage it is entirely read-only and view-scoped, and takes no
// admin-kubeconfig shortcut: a read-role member's own per-user reader credential covers all of it -
// Services and endpoints via the built-in `view` role, the Gateway API group via the cluster-scoped
// read role the kaas:readers group is bound to (see ansible/roles/viewer_kubeconfig; `view` does not
// cover CRDs). So these all reuse workloadRead.
//
// There is deliberately NO add-on gate, unlike Monitoring/Security. The page's headline is the
// platform's north-south contract - the reserved gateway address and the wildcard DNS record - which
// comes off the cluster row and is worth showing on a cluster whose user deselected envoy-gateway;
// the seam reports the missing Gateway API as empty lists rather than an error.

// NetworkOverview returns the cluster's north-south contract (reserved LoadBalancer IP, apps domain
// and wildcard record, wiring markers) alongside what is actually published through it.
func (a *App) NetworkOverview(ctx context.Context, actor *domain.User, id string) (*kube.NetworkOverview, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.NetworkOverview(ctx, c, kc)
}

// Services lists the cluster's Services (namespace == "" means all namespaces).
func (a *App) Services(ctx context.Context, actor *domain.User, id, namespace string) ([]kube.ServiceSummary, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.Services(ctx, c, kc, namespace)
}

// Service returns one Service's detail (its metadata and the endpoints behind it).
func (a *App) Service(ctx context.Context, actor *domain.User, id string, ref kube.ObjectRef) (*kube.ServiceDetail, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.Service(ctx, c, kc, ref)
}

// ServiceManifest returns a Service's YAML.
func (a *App) ServiceManifest(ctx context.Context, actor *domain.User, id string, ref kube.ObjectRef) (string, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return "", err
	}
	return a.Kube.ServiceManifest(ctx, c, kc, ref)
}

// ServiceEvents returns a Service's events (a LoadBalancer with no address shows up here).
func (a *App) ServiceEvents(ctx context.Context, actor *domain.User, id string, ref kube.ObjectRef) ([]kube.Event, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.ServiceEvents(ctx, c, kc, ref)
}

// Gateways lists the cluster's Gateway API Gateways (empty when the add-on isn't installed).
func (a *App) Gateways(ctx context.Context, actor *domain.User, id string) ([]kube.GatewaySummary, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.Gateways(ctx, c, kc)
}

// GatewayManifest returns a Gateway's YAML.
func (a *App) GatewayManifest(ctx context.Context, actor *domain.User, id string, ref kube.ObjectRef) (string, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return "", err
	}
	return a.Kube.GatewayManifest(ctx, c, kc, ref)
}

// Routes lists the cluster's Gateway API routes of every kind (namespace == "" means all).
func (a *App) Routes(ctx context.Context, actor *domain.User, id, namespace string) ([]kube.RouteSummary, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return a.Kube.Routes(ctx, c, kc, namespace)
}

// RouteManifest returns a route's YAML. The kind is part of the ref because the route kinds are
// distinct resources sharing one page.
func (a *App) RouteManifest(ctx context.Context, actor *domain.User, id string, kind kube.RouteKind, ref kube.ObjectRef) (string, error) {
	c, kc, err := a.workloadRead(ctx, actor, id)
	if err != nil {
		return "", err
	}
	return a.Kube.RouteManifest(ctx, c, kc, kind, ref)
}

// --- Monitoring (request-driven PromQL query seam; see internal/monitoring) --

// ErrMonitoringNotEnabled is returned when a cluster is Ready and visible but has no monitoring stack
// (kube-prometheus-stack) installed, so there's nothing to query. The API maps it to 409.
var ErrMonitoringNotEnabled = errors.New("monitoring is not enabled for this cluster - install the kube-prometheus-stack add-on")

// MonitoringTabs returns the static Monitoring tab descriptors (id + title) for the page's tab bar.
func (a *App) MonitoringTabs() []monitoring.TabMeta { return monitoring.TabMetas() }

// Monitoring resolves one Monitoring tab's panels for a cluster. View access is enough (owner,
// admin, or any group-mate); the cluster must be Ready and have the monitoring stack installed. It
// queries with the cluster ADMIN kubeconfig server-side - read-only aggregate telemetry with no
// secret exposure (see internal/monitoring) - regardless of the actor's role. window is the
// time-range picker's selection ("5m"…"12h"); it governs range panels and defaults for an empty or
// unrecognised value (see monitoring.ParseRange).
func (a *App) Monitoring(ctx context.Context, actor *domain.User, id, tab, window string) (*monitoring.TabData, error) {
	c, err := a.authorizeCluster(actor, id)
	if err != nil {
		return nil, err
	}
	if c.Phase != domain.PhaseReady {
		return nil, ErrClusterNotReady
	}
	if !monitoring.Enabled(c) {
		return nil, ErrMonitoringNotEnabled
	}
	kc, err := a.openSecret(id, domain.SecretKubeconfig)
	if err != nil {
		return nil, err
	}
	return a.Monitor.Tab(ctx, c, kc, tab, monitoring.ParseRange(window))
}

// ErrUnknownTunnelApp is returned when the {app} segment names no known monitoring UI. The API maps
// it to 404.
var ErrUnknownTunnelApp = errors.New("unknown monitoring UI")

// TunnelApps lists the in-cluster UIs one portal page can link to (tunnel.SurfaceMonitoring for the
// kube-prometheus-stack trio, tunnel.SurfaceStorage for Longhorn), so the page renders one link per
// app. Static; gating on Ready and on the backing add-on is per-cluster.
func (a *App) TunnelApps(surface string) []tunnel.App { return tunnel.AppsFor(surface) }

// ErrStorageUINotEnabled is returned when a cluster is Ready but has no longhorn add-on, so its
// storage UI does not exist. The API maps it to 409, like the monitoring equivalent.
var ErrStorageUINotEnabled = errors.New("the storage UI needs the longhorn add-on installed on this cluster")

// ServeTunnel reverse-proxies one HTTP request to a cluster's in-cluster monitoring UI
// (Grafana/Prometheus/Alertmanager). The cluster must be Ready with the monitoring stack installed,
// and it proxies with the cluster ADMIN kubeconfig server-side (the `view` role can't reach the
// service proxy; see internal/tunnel).
//
// Access is per-app, because the platform's read/write role has to be honoured by apps that know
// nothing about it: a WriteScoped app (Alertmanager - no auth of its own, and its UI silences alerts)
// demands write access, everything else needs only view. For an app that CAN express our role
// (AuthProxy: Grafana), we resolve it into an Identity - read-role group-mate → Viewer, owner/admin/
// write-role → Editor - which the Proxier hands over as authoritative auth-proxy headers.
//
// It returns an error ONLY for these pre-flight checks; once the proxy starts writing the response,
// any transport failure is handled inside the Proxier, so callers must not write after a nil return.
func (a *App) ServeTunnel(w http.ResponseWriter, r *http.Request, actor *domain.User, id, appID string) error {
	app, ok := tunnel.Lookup(appID)
	if !ok {
		return ErrUnknownTunnelApp
	}
	authorize := a.authorizeCluster
	if app.WriteScoped {
		authorize = a.authorizeClusterWrite // read-role group-mates get ErrForbidden → 403
	}
	c, err := authorize(actor, id)
	if err != nil {
		return err
	}
	if c.Phase != domain.PhaseReady {
		return ErrClusterNotReady
	}
	// Each app is gated on the add-on that actually deploys it - the monitoring trio on
	// kube-prometheus-stack, Longhorn on its own chart. Without this a cluster that deselected one of
	// them would proxy to a Service that does not exist and answer 502 instead of saying why.
	switch app.Surface {
	case tunnel.SurfaceStorage:
		if !hasInstalledAddon(c, longhornAddon) {
			return ErrStorageUINotEnabled
		}
	default:
		if !monitoring.Enabled(c) {
			return ErrMonitoringNotEnabled
		}
	}
	kc, err := a.openSecret(id, domain.SecretKubeconfig)
	if err != nil {
		return err
	}
	a.Tunnel.Serve(w, r, c, app, kc, a.tunnelIdentity(actor, c))
	return nil
}

// tunnelIdentity resolves the app-level identity a tunnel user is signed in as: their portal
// username, and a role mapped from their access to THIS cluster (full → Editor, view → Viewer). It
// is derived server-side from the actor and never from the request, so it is safe for the Proxier to
// present to an AuthProxy app as authoritative.
func (a *App) tunnelIdentity(actor *domain.User, c *domain.Cluster) tunnel.Identity {
	ident := tunnel.Identity{Role: tunnel.RoleViewer}
	if actor != nil {
		ident.User = actor.Username
	}
	if a.accessTo(actor, c) == accessFull {
		ident.Role = tunnel.RoleEditor
	}
	return ident
}

// --- Security (request-driven Trivy CRD query seam; see internal/security) ---

// ErrSecurityNotEnabled is returned when a cluster is Ready and visible but has no Trivy Operator
// add-on installed, so there are no report CRDs to read. The API maps it to 409.
var ErrSecurityNotEnabled = errors.New("security scanning is not enabled for this cluster - install the trivy-operator add-on")

// SecurityKinds returns the static report-kind descriptors (id + title + description) for the page's
// tab bar.
func (a *App) SecurityKinds() []security.KindMeta { return security.KindMetas() }

// securityRead resolves a cluster for a read-only security query: view access (owner, admin, or any
// group-mate), the cluster must be Ready and have the Trivy Operator add-on installed, and it returns
// the ADMIN kubeconfig - the report CRDs live in Trivy's API group, which the built-in `view` role a
// read-role member holds does not cover (see internal/security). The data is read-only
// security-posture metadata; production would mint an RBAC-scoped read token instead.
func (a *App) securityRead(actor *domain.User, id string) (*domain.Cluster, []byte, error) {
	c, err := a.authorizeCluster(actor, id)
	if err != nil {
		return nil, nil, err
	}
	if c.Phase != domain.PhaseReady {
		return nil, nil, ErrClusterNotReady
	}
	if !security.Enabled(c) {
		return nil, nil, ErrSecurityNotEnabled
	}
	kc, err := a.openSecret(id, domain.SecretKubeconfig)
	if err != nil {
		return nil, nil, err
	}
	return c, kc, nil
}

// SecurityOverview assembles the cluster-wide security dashboard (all report kinds rolled up).
func (a *App) SecurityOverview(ctx context.Context, actor *domain.User, id string) (*security.Overview, error) {
	c, kc, err := a.securityRead(actor, id)
	if err != nil {
		return nil, err
	}
	return a.Security.Overview(ctx, c, kc)
}

// SecurityReports lists every report of one kind as a summary row.
func (a *App) SecurityReports(ctx context.Context, actor *domain.User, id string, kind security.Kind) ([]security.Report, error) {
	c, kc, err := a.securityRead(actor, id)
	if err != nil {
		return nil, err
	}
	return a.Security.Reports(ctx, c, kc, kind)
}

// SecurityReport returns one report CR with its full finding list.
func (a *App) SecurityReport(ctx context.Context, actor *domain.User, id string, kind security.Kind, namespace, name string) (*security.ReportDetail, error) {
	c, kc, err := a.securityRead(actor, id)
	if err != nil {
		return nil, err
	}
	return a.Security.Report(ctx, c, kc, kind, namespace, name)
}

// --- Audit (request-driven API-server audit query seam; see internal/audit) ---

// auditRead resolves a cluster for a read-only audit query: view access (owner, admin, or any
// group-mate) and the cluster must be Ready, and it returns the ADMIN kubeconfig - reading the
// kube-apiserver pod's log is not something the built-in `view` role grants (see internal/audit).
// Unlike securityRead there is no add-on gate: audit is baked into every control plane at bootstrap,
// so a Ready cluster is always audit-capable (a pre-audit cluster just returns an empty page).
func (a *App) auditRead(actor *domain.User, id string) (*domain.Cluster, []byte, error) {
	c, err := a.authorizeCluster(actor, id)
	if err != nil {
		return nil, nil, err
	}
	if c.Phase != domain.PhaseReady {
		return nil, nil, ErrClusterNotReady
	}
	kc, err := a.openSecret(id, domain.SecretKubeconfig)
	if err != nil {
		return nil, nil, err
	}
	return c, kc, nil
}

// AuditEvents returns a page of the cluster's API-server audit events, filtered and summarized per q.
func (a *App) AuditEvents(ctx context.Context, actor *domain.User, id string, q audit.Query) (*audit.Page, error) {
	c, kc, err := a.auditRead(actor, id)
	if err != nil {
		return nil, err
	}
	return a.Audit.Events(ctx, c, kc, q)
}

// ukcRefreshBefore re-mints a cached per-user kubeconfig once it has less than this left, so a client
// is never handed a credential about to expire mid-use.
const ukcRefreshBefore = 24 * time.Hour

// userClusterKubeconfig returns a per-user kubeconfig for actor on cluster c: a client certificate
// carrying the actor's OWN identity (CN=username) and their RESOLVED access as the Kubernetes group
// the cluster's RBAC binds (O=kaas:writers → cluster-admin, kaas:readers → view). This is what ties
// cluster API access to the same identity + role the portal resolves, uniformly for local and
// directory (LDAP) accounts - the username is the login either way, and the role comes from accessTo.
// It backs every interactive kubectl surface (the shell, Workloads/Storage, scale) and the download,
// so those actions run - and are audited - as the real user rather than a shared admin/viewer.
//
// It is minted via the kube seam (a CertificateSigningRequest signed by the cluster CA) using the
// cluster's own admin config as the minting authority and as the source of the API server + CA - that
// admin credential is never returned - and memoized (see ukcCache) so a page of Workloads calls mints
// at most once. readOnly marks a reader; notAfter is the cert's expiry. Access is resolved via
// accessTo, so a cluster the actor can't see is store.ErrNotFound, and one whose admin config isn't
// ready yet (still provisioning) yields a clear "not ready" error rather than a 500.
func (a *App) userClusterKubeconfig(ctx context.Context, actor *domain.User, c *domain.Cluster) (kc []byte, readOnly bool, notAfter time.Time, err error) {
	access := a.accessTo(actor, c)
	if access == accessNone {
		return nil, false, time.Time{}, store.ErrNotFound
	}
	role := domain.GroupRoleWrite
	if access == accessView {
		role, readOnly = domain.GroupRoleRead, true
	}
	key := c.ID + "|" + actor.ID + "|" + string(role)
	if kc, notAfter, ok := a.cachedUserKubeconfig(key); ok {
		return kc, readOnly, notAfter, nil
	}
	admin, err := a.openSecret(c.ID, domain.SecretKubeconfig)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, readOnly, time.Time{}, fmt.Errorf("kubeconfig is not ready yet - reconnect in a moment")
		}
		return nil, readOnly, time.Time{}, err
	}
	kc, notAfter, err = a.Kube.MintUserKubeconfig(ctx, c, admin, actor.Username, role, a.userKubeconfigTTL)
	if err != nil {
		return nil, readOnly, time.Time{}, err
	}
	a.storeUserKubeconfig(key, kc, notAfter)
	return kc, readOnly, notAfter, nil
}

// UserKubeconfig mints (and caches) a per-user kubeconfig for actor on cluster id, looked up by ID -
// the entry point the interactive shell uses. See userClusterKubeconfig.
func (a *App) UserKubeconfig(ctx context.Context, actor *domain.User, id string) (kc []byte, readOnly bool, err error) {
	c, err := a.Store.GetCluster(id)
	if err != nil {
		return nil, false, err
	}
	kc, readOnly, _, err = a.userClusterKubeconfig(ctx, actor, c)
	return kc, readOnly, err
}

// DownloadKubeconfig is the tenant-facing download: the same per-user credential the shell and
// Workloads use, plus its expiry so the portal can label it. Delegates to userClusterKubeconfig.
func (a *App) DownloadKubeconfig(ctx context.Context, actor *domain.User, id string) (kc []byte, readOnly bool, notAfter time.Time, err error) {
	c, err := a.Store.GetCluster(id)
	if err != nil {
		return nil, false, time.Time{}, err
	}
	return a.userClusterKubeconfig(ctx, actor, c)
}

// cachedUserKubeconfig returns a memoized per-user kubeconfig if one is present and not near expiry.
func (a *App) cachedUserKubeconfig(key string) (kc []byte, notAfter time.Time, ok bool) {
	a.ukcMu.Lock()
	defer a.ukcMu.Unlock()
	e, present := a.ukcCache[key]
	if !present {
		return nil, time.Time{}, false
	}
	if time.Until(e.notAfter) < ukcRefreshBefore {
		delete(a.ukcCache, key) // stale - force a re-mint
		return nil, time.Time{}, false
	}
	return e.kc, e.notAfter, true
}

// storeUserKubeconfig memoizes a freshly minted per-user kubeconfig.
func (a *App) storeUserKubeconfig(key string, kc []byte, notAfter time.Time) {
	a.ukcMu.Lock()
	defer a.ukcMu.Unlock()
	if a.ukcCache == nil {
		a.ukcCache = make(map[string]cachedKubeconfig)
	}
	a.ukcCache[key] = cachedKubeconfig{kc: kc, notAfter: notAfter}
}

// openSecret fetches and decrypts a per-cluster secret.
func (a *App) openSecret(clusterID string, kind domain.SecretKind) ([]byte, error) {
	ct, err := a.Store.GetSecret(clusterID, kind)
	if err != nil {
		return nil, err
	}
	return a.Secrets.Open(ct)
}

// --- Tenancy: authorization, listing, and users -----------------------------

// GetCluster returns a cluster if actor may see it (owner or admin), else store.ErrNotFound.
func (a *App) GetCluster(actor *domain.User, id string) (*domain.Cluster, error) {
	return a.authorizeCluster(actor, id)
}

// ListClusters returns the actor's own clusters plus their group-mates' (full access, the same as
// authorizeCluster grants per-cluster), or every cluster when the actor is an admin.
func (a *App) ListClusters(actor *domain.User) ([]*domain.Cluster, error) {
	if actor == nil {
		return nil, ErrForbidden
	}
	if actor.IsAdmin {
		return a.Store.ListClusters()
	}
	all, err := a.Store.ListClusters()
	if err != nil {
		return nil, err
	}
	if len(actor.Memberships) == 0 {
		return clustersOwnedBy(all, actor.ID), nil
	}
	// The actor sees a cluster if they own it or share any group with its owner.
	actorGroups := make(map[string]bool, len(actor.Memberships))
	for _, m := range actor.Memberships {
		actorGroups[m.GroupID] = true
	}
	users, err := a.Store.ListUsers()
	if err != nil {
		return nil, err
	}
	sharesGroup := make(map[string]bool, len(users)) // owner ID -> shares a group with the actor
	for _, u := range users {
		if u.ID == actor.ID {
			continue
		}
		for _, m := range u.Memberships {
			if actorGroups[m.GroupID] {
				sharesGroup[u.ID] = true
				break
			}
		}
	}
	out := make([]*domain.Cluster, 0)
	for _, c := range all {
		if c.OwnerID == actor.ID || sharesGroup[c.OwnerID] {
			out = append(out, c)
		}
	}
	return out, nil
}

// clusterAccess is what an actor may do with a cluster, resolved by accessTo. The three levels are
// ordered: none (can't even see it) < view (read-only) < full (owner-equivalent).
type clusterAccess int

const (
	accessNone clusterAccess = iota // not owner, group-mate, or admin - invisible
	accessView                      // read-only: a read-role group-mate of the owner
	accessFull                      // owner, admin, or write-role group-mate - may mutate
)

// accessTo resolves an actor's access to a cluster. Owner and admin always get full access; a
// group-mate of the owner (anyone sharing a group) gets access according to the actor's OWN role in
// the shared group(s) - Write anywhere they share grants full access, otherwise view. Anyone else -
// including an ungrouped non-owner - gets none. Role only ever gates access to OTHER members'
// clusters; a user's own clusters are always accessFull via the owner check.
func (a *App) accessTo(actor *domain.User, c *domain.Cluster) clusterAccess {
	if actor == nil {
		return accessNone
	}
	if actor.IsAdmin || c.OwnerID == actor.ID {
		return accessFull
	}
	if len(actor.Memberships) == 0 {
		return accessNone
	}
	owner, err := a.Store.GetUser(c.OwnerID)
	if err != nil {
		return accessNone
	}
	best := accessNone
	for _, m := range actor.Memberships {
		if !owner.InGroup(m.GroupID) {
			continue // the owner isn't in this group - not a shared membership
		}
		if m.Role == domain.GroupRoleWrite {
			return accessFull // Write in any shared group is the ceiling; short-circuit
		}
		best = accessView
	}
	return best
}

// CanManageCluster reports whether actor may mutate cluster c (owner, admin, or write-role
// group-mate) - the write vs. read boundary. Exposed for surfaces the API authorizes itself; the
// download and shell now serve read-role members too (each with their own per-user reader credential),
// so they resolve access via userClusterKubeconfig rather than gating on this.
func (a *App) CanManageCluster(actor *domain.User, c *domain.Cluster) bool {
	return a.accessTo(actor, c) == accessFull
}

// authorizeCluster loads a cluster and enforces read (view) access - owner, admin, or any
// group-mate of the owner, whatever their role. Cross-tenant access returns store.ErrNotFound (not
// ErrForbidden) so a tenant can't probe for the existence of others' clusters. Mutations go through
// authorizeClusterWrite instead.
func (a *App) authorizeCluster(actor *domain.User, id string) (*domain.Cluster, error) {
	c, err := a.Store.GetCluster(id)
	if err != nil {
		return nil, err
	}
	if a.accessTo(actor, c) == accessNone {
		return nil, store.ErrNotFound
	}
	return c, nil
}

// authorizeClusterWrite loads a cluster and enforces write (manage) access - owner, admin, or a
// write-role group-mate. A cluster the actor can't even see is store.ErrNotFound (as with reads);
// one they can see but only read (a read-role group-mate) is ErrForbidden - there's nothing to hide
// since they already know it exists, so the honest 403 explains why the mutation was refused.
func (a *App) authorizeClusterWrite(actor *domain.User, id string) (*domain.Cluster, error) {
	c, err := a.Store.GetCluster(id)
	if err != nil {
		return nil, err
	}
	switch a.accessTo(actor, c) {
	case accessFull:
		return c, nil
	case accessView:
		return nil, ErrForbidden
	default:
		return nil, store.ErrNotFound
	}
}

// NodeSSHTarget authorizes an in-browser SSH request to a single node and resolves the node row.
//
// It gates on WRITE (authorizeClusterWrite), not view: a read-role group-mate gets ErrForbidden
// (403). That is deliberate and not an escalation - a write-role actor already holds the
// cluster-admin kubeconfig, and a privileged pod on the cluster is root on these same nodes, so
// SSH-as-kaas reaches nothing they could not already reach. See internal/nodessh. A node the cluster
// doesn't have is store.ErrNotFound (404), same as an unknown cluster - the browser named a VM, and a
// wrong name is indistinguishable from one that never existed.
func (a *App) NodeSSHTarget(actor *domain.User, clusterID, vmName string) (*domain.Cluster, *domain.Node, error) {
	c, err := a.authorizeClusterWrite(actor, clusterID)
	if err != nil {
		return nil, nil, err
	}
	for i := range c.Nodes {
		if c.Nodes[i].VMName == vmName {
			return c, &c.Nodes[i], nil
		}
	}
	return nil, nil, store.ErrNotFound
}

// AuditNodeSSH records an SSH session lifecycle event ("opened"/"closed") on the cluster's activity
// timeline, attributed to the actor. This is the live-timeline half of the audit; the durable
// history half is the OpSSH operation (BeginNodeSSHOperation/EndNodeSSHOperation), which additionally
// carries the commands typed. Best-effort: a nil broker (some tests) is a no-op.
func (a *App) AuditNodeSSH(c *domain.Cluster, actor *domain.User, n *domain.Node, action string) {
	if a.Broker == nil {
		return
	}
	a.Broker.Emit(events.Event{
		ClusterID: c.ID,
		TS:        time.Now().UTC(),
		Level:     "info",
		Source:    "ssh",
		Message:   fmt.Sprintf("%s %s an SSH session to node %s (%s)", actor.Username, action, n.VMName, n.IP),
	})
}

// BeginNodeSSHOperation records an in-progress OpSSH operation for a session opening on node n and
// returns its id, so EndNodeSSHOperation can complete it (with the captured commands) on close. This
// is what puts the session in the cluster's Operations history. Best-effort: on a store error it logs
// and returns "" - the session still runs, only its audit row is lost. Generation is the cluster's
// current one purely for display; OpSSH is excluded from the reconciler's generation sweep, so a
// still-open session is never auto-completed (see store.CompleteOperations).
func (a *App) BeginNodeSSHOperation(actor *domain.User, c *domain.Cluster, n *domain.Node) string {
	op := &domain.Operation{
		ID:         newID(),
		ClusterID:  c.ID,
		Kind:       domain.OpSSH,
		Summary:    fmt.Sprintf("SSH session to node %s (%s)", n.VMName, n.IP),
		Generation: c.Generation,
		Status:     domain.OpInProgress,
		StartedAt:  time.Now(),
	}
	if actor != nil {
		op.ActorID = actor.ID
		op.ActorUsername = actor.Username
	}
	if err := a.Store.RecordOperation(op); err != nil {
		a.Log.Warn("record ssh operation", "cluster", c.ID, "err", err)
		return ""
	}
	return op.ID
}

// EndNodeSSHOperation completes the OpSSH operation opID, storing the best-effort list of commands
// typed during the session as its detail (newline-joined). A no-op when opID is empty (Begin failed).
func (a *App) EndNodeSSHOperation(opID string, commands []string, truncated bool) {
	if opID == "" {
		return
	}
	if err := a.Store.CompleteOperation(opID, formatSSHCommands(commands, truncated), time.Now()); err != nil {
		a.Log.Warn("complete ssh operation", "op", opID, "err", err)
	}
}

// formatSSHCommands renders the captured command lines into an operation's detail: one per line, with
// a trailing note if the recorder dropped the overflow. Empty when nothing was captured, which leaves
// the stored detail untouched (see store.CompleteOperation) - so a no-command session shows none.
func formatSSHCommands(commands []string, truncated bool) string {
	if len(commands) == 0 {
		return ""
	}
	detail := strings.Join(commands, "\n")
	if truncated {
		detail += "\n… (further commands were not recorded)"
	}
	return detail
}

// budgetFor returns a user's admission budget ON ONE INFRASTRUCTURE. Non-admins draw from their
// own grant for that provider; admins have no fixed slice - they draw directly from whatever that
// provider's ceiling leaves unallocated after every non-admin grant on it (see
// quota.Budget.Unallocated), so an admin never needs to be manually topped down before granting
// quota to someone else.
//
// Quota is per-provider because capacity is: a tenant granted vSphere capacity has no claim on the
// KVM host, and vice versa.
func (a *App) budgetFor(u *domain.User, provider string) (quota.Budget, error) {
	// Shared-pool mode: nobody holds a personal slice - every account (admin or not) draws from the
	// whole ceiling of the provider it's building on. The aggregate checkProviderCapacity gate still
	// bounds real usage to that ceiling, so this is first-come-first-served, not oversubscription.
	if a.SharedQuota {
		return a.providerCeiling(provider), nil
	}
	if !u.IsAdmin {
		q := u.QuotaOn(provider)
		return quota.Budget{TotalVCPU: q.VCPU, TotalMemMB: q.MemMB, TotalDiskGB: q.DiskGB}, nil
	}
	users, err := a.Store.ListUsers()
	if err != nil {
		return quota.Budget{}, err
	}
	vcpu, memMB, diskGB := a.providerCeiling(provider).Unallocated(users, provider)
	return quota.Budget{TotalVCPU: vcpu, TotalMemMB: memMB, TotalDiskGB: diskGB}, nil
}

// providerCeiling is one infrastructure's platform-wide capacity. A provider with no configured
// ceiling (an App built without New - tests) is uncapped in the sense that checkProviderCapacity
// skips it, but an ADMIN's budget there would then be unbounded, which is the safe reading: the
// operator simply hasn't declared a limit for that backend.
func (a *App) providerCeiling(provider string) quota.Budget {
	return a.ProviderBudgets[provider]
}

// ownerBudget returns the owner's budget on the cluster's OWN infrastructure, plus the owner's
// clusters on that same infrastructure - so an edit is charged to the owner (even when an admin or
// group-mate performs it) against the right backend's grant. A missing owner record (e.g. a
// cluster orphaned by a deleted user) yields a zero budget, which blocks growth - the safe default.
func (a *App) ownerBudget(ownerID, provider string) (quota.Budget, []*domain.Cluster, error) {
	owner, err := a.Store.GetUser(ownerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return quota.Budget{}, nil, nil
		}
		return quota.Budget{}, nil, err
	}
	budget, err := a.budgetFor(owner, provider)
	if err != nil {
		return quota.Budget{}, nil, err
	}
	owned, err := a.Store.ListClustersByOwner(ownerID)
	if err != nil {
		return quota.Budget{}, nil, err
	}
	return budget, clustersOnProvider(owned, provider), nil
}

// Register creates a self-service tenant account with zero quota (an admin grants quota later).
//
// Disabled entirely when the deployment authenticates against a directory: accounts there are
// provisioned from the directory on first login, so a self-service form could only ever create a
// local account the directory has never heard of - which is both confusing and a way to squat a
// colleague's username before they first log in.
func (a *App) Register(username, password string) (*domain.User, error) {
	if a.Authn != nil {
		return nil, ErrRegistrationDisabled
	}
	username, err := validateUsername(username)
	if err != nil {
		return nil, err
	}
	if len(password) < 6 {
		return nil, fmt.Errorf("password must be at least 6 characters")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, err
	}
	u := &domain.User{ID: newID(), Username: username, PasswordHash: hash, CreatedAt: time.Now()}
	if err := a.Store.CreateUser(u); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, fmt.Errorf("username %q is taken", username)
		}
		return nil, err
	}
	return u, nil
}

// Login verifies credentials and returns the user. A wrong username or password both return
// ErrInvalidCredentials, so the RESPONSE doesn't reveal which accounts exist.
//
// (In ldap mode the response is still uniform, but the response TIME is not: a local account costs
// a bcrypt compare, an unknown one costs a round trip to a domain controller. Closing that channel
// would mean always doing both, on every login. We accept the oracle instead - see
// docs/architecture.md's "production would…" list.)
//
// Local accounts are tried FIRST, and this is the break-glass guarantee: the seeded admin
// authenticates with its own password even when the directory is unreachable, which is what keeps
// `make kubeconfig` and deploy/teardown-clusters.sh working, and what stops a DC outage from
// locking everyone out of the control plane. A directory account never matches here - its stored
// hash is empty, and bcrypt against an empty hash always fails.
//
// clientIP is used only for throttling and may be empty.
func (a *App) Login(ctx context.Context, username, password, clientIP string) (*domain.User, error) {
	username = strings.TrimSpace(username)
	if u, err := a.Store.GetUserByUsername(username); err == nil {
		// Deliberately NOT lowercased before the lookup. validateUsername already forces
		// self-registered names to lowercase, so the only account that could be mixed-case is the
		// seeded admin - whose name comes from KAAS_ADMIN_USERNAME and is never validated.
		// Normalizing here would lock a `KAAS_ADMIN_USERNAME=Admin` deployment out of break-glass
		// permanently, which is precisely the account that must never become unreachable.
		if u.PasswordHash != "" && auth.CheckPassword(u.PasswordHash, password) {
			return u, nil
		}
	}
	if a.Authn == nil {
		return nil, ErrInvalidCredentials
	}
	// The break-glass admin's name is owned by the local account, exclusively. If the directory were
	// allowed to claim it, an operator who set KAAS_ADMIN_USERNAME to a real person's name would
	// have handed platform admin to anyone holding KAAS_ADMIN_PASSWORD - which defaults to "admin".
	// Their local password is the only way in for that name; a directory password must not be.
	if seededAdminName(username) {
		a.Log.Warn("directory login refused for the seeded admin username - it is a local account",
			"username", username)
		return nil, ErrInvalidCredentials
	}
	// Throttle BEFORE the directory call, never after: the point is to not send the bad password to
	// the DC at all. Every failed bind here counts against the real account's AD lockout policy, and
	// /auth/login is public and unauthenticated - so without this, anyone who can reach the portal
	// could lock out every account in the domain by spraying a name list.
	if err := a.throttle().check(username, clientIP); err != nil {
		return nil, err
	}
	id, err := a.Authn.Authenticate(ctx, username, password)
	if err != nil {
		if errors.Is(err, authn.ErrInvalidCredentials) {
			a.throttle().recordFailure(username, clientIP)
			return nil, ErrInvalidCredentials
		}
		// An unreachable directory or a broken service account is the platform's fault. Reporting it
		// as a bad password would send users off to reset a password that was never wrong - and it
		// must not count against the throttle either.
		a.Log.Error("directory authentication failed", "username", username, "err", err)
		return nil, err
	}
	u, err := a.syncDirectoryUser(id)
	if err != nil {
		if errors.Is(err, errLocalAccountCollision) {
			// Opaque to the caller on purpose: "that name is a local account" tells an attacker
			// exactly which names to attack with a password guess instead.
			a.Log.Warn("directory identity collides with a local account", "username", id.Username)
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	a.throttle().reset(username, clientIP)
	return u, nil
}

// IssueSession returns a signed session token for a user (carried in the kaas_session cookie).
func (a *App) IssueSession(userID string) string {
	return a.Signer.Issue(userID, SessionTTL)
}

// ResolveSession validates a session token and loads the user it names. Returns store.ErrNotFound
// when the token is valid but the user has since been deleted, so the API treats it as logged-out.
func (a *App) ResolveSession(token string) (*domain.User, error) {
	userID, err := a.Signer.Verify(token)
	if err != nil {
		return nil, err
	}
	return a.Store.GetUser(userID)
}

// UserView is a user plus their rolled-up cluster usage, for the admin users table. Usage is
// broken out per infrastructure to sit alongside User.Quotas (the grant per infrastructure) -
// together they are the "granted vs. used, on each backend" pair the admin actually manages.
type UserView struct {
	domain.User
	UsedVCPU     int                             `json:"used_vcpu"`
	UsedMemMB    int                             `json:"used_mem_mb"`
	UsedDiskGB   int                             `json:"used_disk_gb"`
	ClusterCount int                             `json:"cluster_count"`
	Usage        map[string]domain.ResourceQuota `json:"usage,omitempty"`
}

// ProviderAllocation is one infrastructure's conserved pool: its ceiling, how much of it is already
// granted to non-admin accounts, and how much the admins' own live clusters are already consuming
// out of the rest. What's genuinely free to grant is Total − Allocated − AdminUsed: admins hold no
// stored slice, but their running clusters draw from the same pool, so the unallocated remainder
// overstates true headroom until their usage is subtracted (see budgetFor / quota.Unallocated).
type ProviderAllocation struct {
	Provider        string `json:"provider"`
	TotalVCPU       int    `json:"total_vcpu"`
	TotalMemMB      int    `json:"total_mem_mb"`
	TotalDiskGB     int    `json:"total_disk_gb"`
	AllocatedVCPU   int    `json:"allocated_vcpu"`
	AllocatedMemMB  int    `json:"allocated_mem_mb"`
	AllocatedDiskGB int    `json:"allocated_disk_gb"`
	// AdminUsed* is the live vCPU/memory/disk consumed by clusters owned by ADMIN accounts on this
	// backend. Admins draw straight from the unallocated pool, so this capacity is spoken for even
	// though it never appears as a grant.
	AdminUsedVCPU   int `json:"admin_used_vcpu"`
	AdminUsedMemMB  int `json:"admin_used_mem_mb"`
	AdminUsedDiskGB int `json:"admin_used_disk_gb"`
}

// UsersReport is the admin dashboard payload: every user with usage, plus the allocation summary.
//
// Allocation - one entry per infrastructure - is the operative part: quota is granted, and the
// conserved-pool invariant enforced, per backend. The top-level Total*/Allocated* are the sums,
// for a headline only; no grant is ever checked against them.
type UsersReport struct {
	Users           []UserView           `json:"users"`
	TotalVCPU       int                  `json:"total_vcpu"`
	TotalMemMB      int                  `json:"total_mem_mb"`
	TotalDiskGB     int                  `json:"total_disk_gb"`
	AllocatedVCPU   int                  `json:"allocated_vcpu"`
	AllocatedMemMB  int                  `json:"allocated_mem_mb"`
	AllocatedDiskGB int                  `json:"allocated_disk_gb"`
	Allocation      []ProviderAllocation `json:"allocation"`
	// SharedQuota mirrors App.SharedQuota: when true, per-user grants are off and every account
	// draws from each backend's full ceiling. The Allocated* figures above then reflect only dormant
	// stored grants and are not meaningful - the Admin page hides the grant editor and shows each
	// user's consumption of the shared pool instead. See budgetFor.
	SharedQuota bool `json:"shared_quota"`
}

// ListUsers returns every account with usage and the platform allocation summary. Admin only.
func (a *App) ListUsers(actor *domain.User) (*UsersReport, error) {
	if actor == nil || !actor.IsAdmin {
		return nil, ErrForbidden
	}
	users, err := a.Store.ListUsers()
	if err != nil {
		return nil, err
	}
	allClusters, err := a.Store.ListClusters()
	if err != nil {
		return nil, err
	}
	providers := a.infraProviders()
	// adminUsed accumulates the live usage of clusters owned by admin accounts, per provider - the
	// capacity admins draw straight from each backend's unallocated pool (they hold no grant).
	adminUsed := make(map[string]domain.ResourceQuota, len(providers))
	views := make([]UserView, 0, len(users))
	for _, u := range users {
		owned := clustersOwnedBy(allClusters, u.ID)
		cpu, mem, disk := quota.Usage(owned)
		usage := make(map[string]domain.ResourceQuota, len(providers))
		for _, p := range providers {
			pcpu, pmem, pdisk := quota.Usage(clustersOnProvider(owned, p))
			usage[p] = domain.ResourceQuota{VCPU: pcpu, MemMB: pmem, DiskGB: pdisk}
			if u.IsAdmin {
				au := adminUsed[p]
				adminUsed[p] = domain.ResourceQuota{VCPU: au.VCPU + pcpu, MemMB: au.MemMB + pmem, DiskGB: au.DiskGB + pdisk}
			}
		}
		views = append(views, UserView{
			User: *u, UsedVCPU: cpu, UsedMemMB: mem, UsedDiskGB: disk,
			ClusterCount: len(owned), Usage: usage,
		})
	}

	rep := &UsersReport{Users: views, SharedQuota: a.SharedQuota}
	for _, p := range providers {
		ceiling := a.providerCeiling(p)
		allocCPU, allocMem, allocDisk := quota.Allocated(users, p)
		au := adminUsed[p]
		rep.Allocation = append(rep.Allocation, ProviderAllocation{
			Provider:        p,
			TotalVCPU:       ceiling.TotalVCPU,
			TotalMemMB:      ceiling.TotalMemMB,
			TotalDiskGB:     ceiling.TotalDiskGB,
			AllocatedVCPU:   allocCPU,
			AllocatedMemMB:  allocMem,
			AllocatedDiskGB: allocDisk,
			AdminUsedVCPU:   au.VCPU,
			AdminUsedMemMB:  au.MemMB,
			AdminUsedDiskGB: au.DiskGB,
		})
		rep.TotalVCPU += ceiling.TotalVCPU
		rep.TotalMemMB += ceiling.TotalMemMB
		rep.TotalDiskGB += ceiling.TotalDiskGB
		rep.AllocatedVCPU += allocCPU
		rep.AllocatedMemMB += allocMem
		rep.AllocatedDiskGB += allocDisk
	}
	return rep, nil
}

// UpdateUserRequest is an admin edit to an account: quota, group memberships, or both. Nil/pointer
// fields are left unchanged; a non-nil Memberships replaces the user's ENTIRE membership set (an
// empty slice removes them from every group).
type UpdateUserRequest struct {
	// Quotas, when non-nil, is the user's grant PER INFRASTRUCTURE, keyed by provider. It is a
	// merge, not a replace: only the providers present are changed, so an admin can top up one
	// backend without restating the others. Set a provider to zero to revoke its capacity.
	Quotas *map[string]domain.ResourceQuota `json:"quotas,omitempty"`
	// Memberships, when non-nil, is the full desired set of (group, role) memberships for the user -
	// the update is a wholesale replace, not a merge, so the UI sends the complete list every time.
	Memberships *[]domain.GroupMembership `json:"memberships,omitempty"`
}

// UpdateUser applies a quota grant, a group-membership change, or both, to an account. Admin only.
//
// Quota is granted per infrastructure, and the conserved-pool invariant is enforced per
// infrastructure (sum of all NON-ADMIN grants on a backend <= that backend's ceiling - see
// quota.Budget.CheckAllocation). A grant is rejected for an admin target, since an admin's budget
// on each backend is always that backend's live unallocated pool, not a stored slice (see
// budgetFor). Lowering a grant below current usage is allowed - existing clusters keep running,
// the user just can't grow past the new cap.
//
// Memberships (when non-nil) replaces the user's whole membership set. Each entry's group must exist
// and its role must be valid ("read" | "write"); duplicate groups are rejected. A user can be in
// several groups at once with an independent role in each (see domain.GroupMembership). Only an admin
// can change memberships (this whole method is admin-only), so a user can never escalate their own
// access.
func (a *App) UpdateUser(actor *domain.User, userID string, req UpdateUserRequest) (*domain.User, error) {
	if actor == nil || !actor.IsAdmin {
		return nil, ErrForbidden
	}
	var out *domain.User
	err := a.Store.WithLock(store.LockAdmission, func() error {
		u, err := a.updateUserLocked(userID, req)
		out = u
		return err
	})
	return out, err
}

// updateUserLocked is UpdateUser's body, minus the lock. Callers must already hold LockAdmission -
// and must NOT be inside any other WithLock, which would deadlock (see lockUserWrite's doc).
func (a *App) updateUserLocked(userID string, req UpdateUserRequest) (*domain.User, error) {
	u, err := a.Store.GetUser(userID)
	if err != nil {
		return nil, err
	}
	if req.Quotas != nil {
		if a.SharedQuota {
			return nil, fmt.Errorf("per-user quota is disabled (KAAS_SHARED_QUOTA): every account draws from each infrastructure's full ceiling automatically")
		}
		if u.IsAdmin {
			return nil, fmt.Errorf("admin accounts draw from each infrastructure's unallocated pool automatically; quota can't be set directly")
		}
		users, err := a.Store.ListUsers()
		if err != nil {
			return nil, err
		}
		enabled := a.infraProviders()
		next := maps.Clone(u.Quotas)
		if next == nil {
			next = map[string]domain.ResourceQuota{}
		}
		for provider, g := range *req.Quotas {
			if !slices.Contains(enabled, provider) {
				return nil, fmt.Errorf("infrastructure provider %q is not enabled (available: %s)", provider, strings.Join(enabled, ", "))
			}
			// Each backend's pool is conserved on its own: granting vSphere capacity can't be
			// funded out of the KVM host's spare cores.
			if err := a.providerCeiling(provider).CheckAllocation(users, userID, provider, g.VCPU, g.MemMB, g.DiskGB); err != nil {
				return nil, err
			}
			next[provider] = g
		}
		u.Quotas = next
	}
	if req.Memberships != nil {
		// One ListGroups pass, rather than the GetGroup-per-membership this used to do: the merge
		// below needs every group's source anyway, so the N+1 buys nothing.
		groups, err := a.groupsByID()
		if err != nil {
			return nil, err
		}
		seen := make(map[string]bool, len(*req.Memberships))
		requested := make([]domain.GroupMembership, 0, len(*req.Memberships))
		for _, m := range *req.Memberships {
			if m.GroupID == "" {
				return nil, fmt.Errorf("membership is missing a group id")
			}
			if seen[m.GroupID] {
				return nil, fmt.Errorf("duplicate membership for group %q", m.GroupID)
			}
			seen[m.GroupID] = true
			if !m.Role.Valid() {
				return nil, fmt.Errorf("invalid group role %q (want read|write)", m.Role)
			}
			if _, ok := groups[m.GroupID]; !ok {
				return nil, fmt.Errorf("unknown group %q", m.GroupID)
			}
			requested = append(requested, m)
		}
		// The request is authoritative for local groups only; the user's existing directory
		// memberships are carried through untouched (see mergeMemberships).
		u.Memberships = mergeMemberships(u.Memberships, requested, groups)
	}
	if err := a.Store.UpdateUser(u); err != nil {
		return nil, err
	}
	return u, nil
}

// groupsByID loads every group once, keyed by ID.
func (a *App) groupsByID() (map[string]*domain.Group, error) {
	all, err := a.Store.ListGroups()
	if err != nil {
		return nil, err
	}
	out := make(map[string]*domain.Group, len(all))
	for _, g := range all {
		out[g.ID] = g
	}
	return out, nil
}

// mergeMemberships splits a user's memberships by who OWNS each group and takes each half from the
// side that is authoritative for it:
//
//	memberships in directory groups ← directorySide (the mapping rules own these)
//	memberships in local groups     ← localSide     (admins own these)
//
// Both writers call this, passing themselves as their own side, which is what lets a user hold
// directory-driven and admin-assigned memberships at once without either writer clobbering the
// other's:
//
//	UpdateUser         merge(u.Memberships, requested, …)  - trusts the request only for local groups
//	syncDirectoryUser  merge(claimed, u.Memberships, …)    - trusts the directory only for its own
//
// It is a merge rather than a validate-and-reject for a concrete reason: the Admin page sends the
// user's ENTIRE membership list on every edit (it has no diff to send), directory memberships
// included. Rejecting a payload that mentions a directory group would make it impossible to add a
// directory user to a local group - the very thing an admin most obviously wants to do.
//
// It also makes both writes idempotent against a stale caller. An admin whose cached UserView
// predates a login cannot clobber what that login just synced, because the directory half is never
// read from the request; and a login cannot revert an admin's group assignment, because the local
// half is never read from the directory.
//
// A membership naming an unknown group is dropped: groups[id] misses, so it is neither
// directory-managed (first loop skips it) nor carried over (second loop skips it). UpdateUser
// rejects those up front; this is the backstop for a group deleted under a live session.
func mergeMemberships(directorySide, localSide []domain.GroupMembership, groups map[string]*domain.Group) []domain.GroupMembership {
	out := make([]domain.GroupMembership, 0, len(directorySide)+len(localSide))
	for _, m := range directorySide {
		if g, ok := groups[m.GroupID]; ok && g.DirectoryManaged() {
			out = append(out, m)
		}
	}
	for _, m := range localSide {
		if g, ok := groups[m.GroupID]; ok && !g.DirectoryManaged() {
			out = append(out, m)
		}
	}
	sortMemberships(out)
	return out
}

// sortMemberships orders a membership set by group ID. The store rewrites the set wholesale, so a
// stable order keeps writes and test comparisons deterministic.
func sortMemberships(ms []domain.GroupMembership) {
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].GroupID < ms[j].GroupID })
}

// GroupView is a group plus its members' usernames, for the admin groups table.
type GroupView struct {
	domain.Group
	Members []string `json:"members"`
	// Orphaned marks a directory group whose mapping rule has been removed from the config. Nothing
	// syncs it any more and nothing would recreate it, so - unlike a live directory group - the
	// portal lets an admin rename or delete it. Removing a rule deliberately leaves the group
	// standing (a config typo must not destroy a team), which is exactly why this state exists and
	// has to be visible rather than merely tolerated. Always false for local groups.
	Orphaned bool `json:"orphaned,omitempty"`
}

// CreateGroup creates an admin-managed team. Admin only.
func (a *App) CreateGroup(actor *domain.User, name string) (*domain.Group, error) {
	if actor == nil || !actor.IsAdmin {
		return nil, ErrForbidden
	}
	name, err := validateGroupName(name)
	if err != nil {
		return nil, err
	}
	g := &domain.Group{ID: newID(), Name: name, CreatedAt: time.Now()}
	if err := a.Store.CreateGroup(g); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, fmt.Errorf("group %q already exists", name)
		}
		return nil, err
	}
	return g, nil
}

// ListGroups returns every group with its members' usernames. Admin only.
func (a *App) ListGroups(actor *domain.User) ([]GroupView, error) {
	if actor == nil || !actor.IsAdmin {
		return nil, ErrForbidden
	}
	groups, err := a.Store.ListGroups()
	if err != nil {
		return nil, err
	}
	users, err := a.Store.ListUsers()
	if err != nil {
		return nil, err
	}
	views := make([]GroupView, 0, len(groups))
	for _, g := range groups {
		var members []string
		for _, u := range users {
			if u.InGroup(g.ID) {
				members = append(members, u.Username)
			}
		}
		views = append(views, GroupView{
			Group:    *g,
			Members:  members,
			Orphaned: g.DirectoryManaged() && !a.directoryRuleFor(g),
		})
	}
	return views, nil
}

// RenameGroup renames an existing group. Admin only, and never a group a live mapping rule claims.
//
// Such a group is refused because its identity lives in ldap.yaml: renaming the row here would be
// undone by the next boot's seeding pass, which relabels the group back to whatever its rule says.
// Rename the rule's `group:` in the config instead - seeding follows it, because groups are keyed on
// `group_key` rather than the display name.
//
// A group ORPHANED by a config edit (its rule is gone) is renameable: nothing will overwrite it, so
// it is the admins' to deal with. See directoryRuleFor.
func (a *App) RenameGroup(actor *domain.User, id, name string) (*domain.Group, error) {
	if actor == nil || !actor.IsAdmin {
		return nil, ErrForbidden
	}
	name, err := validateGroupName(name)
	if err != nil {
		return nil, err
	}
	g, err := a.Store.GetGroup(id)
	if err != nil {
		return nil, err
	}
	if a.directoryRuleFor(g) {
		return nil, fmt.Errorf("%w: group %q is managed by the directory - rename it in the ldap mapping config instead (change its `group:`, keeping `group_key` the same)", ErrForbidden, g.Name)
	}
	g.Name = name
	if err := a.Store.UpdateGroup(g); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, fmt.Errorf("group %q already exists", name)
		}
		return nil, err
	}
	return g, nil
}

// DeleteGroup removes a group and drops it from every member's memberships - it never touches their
// clusters (or their other group memberships). Admin only, and never a group a live mapping rule
// claims.
//
// Such a group is refused for the same reason RenameGroup refuses one: the next boot would recreate
// it from its rule with a fresh ID, so the "delete" would be a no-op that had meanwhile stripped
// everyone's membership. Remove the rule from ldap.yaml instead.
//
// Deleting the RULE does not delete the group - that would let a typo in a config file destroy a
// team - so the group is left behind, orphaned. This is where an admin cleans that up: an orphan has
// no rule to recreate it, so it is deletable like any local group.
func (a *App) DeleteGroup(actor *domain.User, id string) error {
	if actor == nil || !actor.IsAdmin {
		return ErrForbidden
	}
	return a.Store.WithLock(store.LockAdmission, func() error { return a.deleteGroupLocked(id) })
}

// deleteGroupLocked is DeleteGroup's body. Caller must hold LockAdmission: this rewrites a user row
// per member, and an unlocked pass would race a concurrent login's membership sync (or an admin's
// quota grant) and lose whichever write got there first.
func (a *App) deleteGroupLocked(id string) error {
	g, err := a.Store.GetGroup(id)
	if err != nil {
		return err
	}
	if a.directoryRuleFor(g) {
		return fmt.Errorf("%w: group %q is managed by the directory - remove its rule from the ldap mapping config instead", ErrForbidden, g.Name)
	}
	users, err := a.Store.ListUsers()
	if err != nil {
		return err
	}
	for _, u := range users {
		if !u.InGroup(id) {
			continue
		}
		kept := make([]domain.GroupMembership, 0, len(u.Memberships))
		for _, m := range u.Memberships {
			if m.GroupID != id {
				kept = append(kept, m)
			}
		}
		u.Memberships = kept
		if err := a.Store.UpdateUser(u); err != nil {
			return err
		}
	}
	return a.Store.DeleteGroup(id)
}

// OwnerUsernames resolves owner IDs to usernames for a set of clusters in one pass, so cluster
// list/detail responses can show who requested each one without a separate (admin-only) users
// call. Safe to expose broadly: it only reveals the username of an owner whose cluster the caller
// already has visibility into.
func (a *App) OwnerUsernames(clusters []*domain.Cluster) (map[string]string, error) {
	ids := make(map[string]bool, len(clusters))
	for _, c := range clusters {
		ids[c.OwnerID] = true
	}
	users, err := a.Store.ListUsers()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(ids))
	for _, u := range users {
		if ids[u.ID] {
			out[u.ID] = u.Username
		}
	}
	return out, nil
}

// validateGroupName trims and checks a group name: 2–40 characters, non-empty after trim. Display
// label, not a login credential, so (unlike validateUsername) any printable characters are fine.
func validateGroupName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if len(name) < 2 || len(name) > 40 {
		return "", fmt.Errorf("group name must be 2–40 characters")
	}
	return name, nil
}

// DeleteUser removes an account and cascades: its clusters are moved to Deleting (the reconciler
// tears down the real VMs and quota is reclaimed as they terminate). Admin only. Guards prevent
// deleting your own account or the last remaining admin.
func (a *App) DeleteUser(actor *domain.User, userID string) error {
	if actor == nil || !actor.IsAdmin {
		return ErrForbidden
	}
	if actor.ID == userID {
		return fmt.Errorf("cannot delete your own account")
	}
	return a.Store.WithLock(store.LockAdmission, func() error { return a.deleteUserLocked(userID) })
}

// deleteUserLocked is DeleteUser's body. Caller must hold LockAdmission - deleting an account frees
// its quota grant, which is a write to the conserved pool every other admission is reading.
func (a *App) deleteUserLocked(userID string) error {
	target, err := a.Store.GetUser(userID)
	if err != nil {
		return err
	}
	if target.IsAdmin {
		n, err := a.countAdmins()
		if err != nil {
			return err
		}
		if n <= 1 {
			return fmt.Errorf("cannot delete the last admin account")
		}
	}
	owned, err := a.Store.ListClustersByOwner(userID)
	if err != nil {
		return err
	}
	for _, c := range owned {
		if c.Phase == domain.PhaseDeleting || c.Phase.Terminal() {
			continue
		}
		c.Phase = domain.PhaseDeleting
		c.Generation++
		if err := a.Store.UpdateCluster(c); err != nil {
			return err
		}
	}
	return a.Store.DeleteUser(userID)
}

// countAdmins counts admin accounts that can still log in WITHOUT a directory - i.e. the
// break-glass admins.
//
// The local-only filter is what makes "you can't delete the last admin" mean "you can't lock
// yourself out". Counting directory admins too would satisfy the guard with accounts that stop
// working the moment the DCs are unreachable, or the moment someone edits a mapping rule - and
// `make kubeconfig` and deploy/teardown-clusters.sh authenticate as the local admin, so they'd go
// with it.
//
// Today no directory account can be an admin at all (domain.User.IsAdmin is seed-only, and mapping
// rules deliberately grant group roles only), so this filter is currently a no-op. It is here so
// that if that ever changes, the guarantee doesn't quietly evaporate with it.
func (a *App) countAdmins() (int, error) {
	users, err := a.Store.ListUsers()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, u := range users {
		if u.IsAdmin && !u.FromDirectory() {
			n++
		}
	}
	return n, nil
}

// ensureAdmin seeds the admin account (idempotently) and takes ownership of any clusters that
// predate multi-tenancy. Called from New by EVERY api and worker replica, all of which boot at
// once - so the whole thing runs under a platform-wide lock: both halves are read-then-writes
// (does the admin exist? which clusters are unowned?) and the losers of an unserialized race would
// fail startup on a duplicate username, or double-assign owners. Serialized, the first replica
// seeds and the rest find the work already done.
func (a *App) ensureAdmin() error {
	return a.Store.WithLock(store.LockUserSeed, a.ensureAdminLocked)
}

func (a *App) ensureAdminLocked() error {
	username := getenv("KAAS_ADMIN_USERNAME", "admin")
	if existing, err := a.Store.GetUserByUsername(username); err == nil {
		return a.backfillOwners(existing.ID)
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	password := os.Getenv("KAAS_ADMIN_PASSWORD")
	if password == "" {
		password = "admin"
		a.Log.Warn("KAAS_ADMIN_PASSWORD not set - seeding admin with the default password \"admin\"; set it for anything real")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	// The admin holds no stored quota - its budget is always the live unallocated pool (see
	// budgetFor), so it can create clusters out of the box and never needs to be manually topped
	// down before granting quota to a tenant.
	admin := &domain.User{
		ID:           newID(),
		Username:     username,
		PasswordHash: hash,
		IsAdmin:      true,
		CreatedAt:    time.Now(),
	}
	if err := a.Store.CreateUser(admin); err != nil {
		if errors.Is(err, store.ErrConflict) { // lost a race with another process; the admin now exists
			existing, gerr := a.Store.GetUserByUsername(username)
			if gerr != nil {
				return gerr
			}
			return a.backfillOwners(existing.ID)
		}
		return err
	}
	a.Log.Info("seeded admin account", "username", username)
	return a.backfillOwners(admin.ID)
}

// AdminUser returns the seeded admin account (by KAAS_ADMIN_USERNAME). Useful for headless callers
// like the worker's demo seed, which act as the platform admin.
func (a *App) AdminUser() (*domain.User, error) {
	return a.Store.GetUserByUsername(getenv("KAAS_ADMIN_USERNAME", "admin"))
}

// backfillOwners assigns any unowned cluster (created before tenancy) to the admin.
func (a *App) backfillOwners(adminID string) error {
	all, err := a.Store.ListClusters()
	if err != nil {
		return err
	}
	for _, c := range all {
		if c.OwnerID == "" {
			c.OwnerID = adminID
			if err := a.Store.UpdateCluster(c); err != nil {
				return err
			}
		}
	}
	return nil
}

func clustersOwnedBy(list []*domain.Cluster, ownerID string) []*domain.Cluster {
	out := make([]*domain.Cluster, 0)
	for _, c := range list {
		if c.OwnerID == ownerID {
			out = append(out, c)
		}
	}
	return out
}

// validateUsername trims and checks a SELF-CHOSEN username: 3–32 chars of lowercase letters,
// digits, '-' or '_'. Deliberately strict - this one names an account someone is registering, so we
// get to insist. Directory-supplied names go through validateDirectoryUsername instead.
func validateUsername(username string) (string, error) {
	username = strings.TrimSpace(username)
	if len(username) < 3 || len(username) > 32 {
		return "", fmt.Errorf("username must be 3–32 characters")
	}
	for _, r := range username {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' {
			return "", fmt.Errorf("username may contain only lowercase letters, digits, '-' and '_'")
		}
	}
	return username, nil
}

// validateDirectoryUsername checks a name the DIRECTORY chose, so it is looser than
// validateUsername by necessity rather than by preference: we do not get to tell Active Directory
// what its sAMAccountNames look like, and rejecting one means a real employee simply cannot log in.
// Real names carry dots ("d.vaz"), and a userPrincipalName-based config yields full
// "dvaz@example.lab" logins - both of which validateUsername rejects outright.
//
// The input is already lowercased by the authenticator (which reads it from the configured username
// attribute, not from what was typed), so this validates rather than normalizes.
//
// It is not merely cosmetic: the value becomes a portal username, which is compared, displayed and
// carried in a session. Anything with whitespace or control characters is refused rather than
// stored.
func validateDirectoryUsername(username string) (string, error) {
	username = strings.TrimSpace(username)
	if len(username) < 3 || len(username) > 64 {
		return "", fmt.Errorf("username must be 3–64 characters, got %d", len(username))
	}
	for _, r := range username {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '@':
		default:
			return "", fmt.Errorf("username may contain only lowercase letters, digits, '-', '_', '.' and '@' (got %q)", r)
		}
	}
	return username, nil
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// loadKey derives a 32-byte AES key from KAAS_SECRET_KEY, or generates an ephemeral one.
func loadKey(log *slog.Logger) []byte {
	if v := os.Getenv("KAAS_SECRET_KEY"); v != "" {
		sum := sha256.Sum256([]byte(v))
		return sum[:]
	}
	log.Warn("KAAS_SECRET_KEY not set - using an ephemeral key (secrets won't survive restart)")
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envFloat reads a fractional tunable (today: the etcd fragmentation threshold), forgiving a
// malformed value the same way envInt does. Out-of-range values are the caller's business to clamp.
func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool parses a boolean tunable, accepting the usual truthy spellings (1/true/yes/on, any case)
// and falling back to def on empty or unparseable - matching envInt's forgiving style.
func envBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// envDuration parses a Go duration ("5m", "30s"), falling back to def on anything unparseable -
// matching envInt's forgiving style rather than failing a boot over a typo'd tunable.
func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// certRenewWindow is the reconciler's CertRenewWindow (automatic control-plane certificate rotation)
// from env: KAAS_CERT_RENEW gates the feature (default on) and KAAS_CERT_RENEW_WINDOW sets how close
// to expiry rotation fires (default 30 days). Returns 0 when disabled, which turns the feature off in
// the reconciler entirely - no cert observation, no renewal.
func certRenewWindow() time.Duration {
	if !envBool("KAAS_CERT_RENEW", true) {
		return 0
	}
	return envDuration("KAAS_CERT_RENEW_WINDOW", 30*24*time.Hour)
}

// etcdMinQuotaBytes / etcdMaxQuotaBytes bound KAAS_ETCD_QUOTA_BYTES. The upper bound is etcd's own
// documented maximum for --quota-backend-bytes; above it etcd warns and behaviour is unsupported.
// The lower bound is etcd's default - this knob exists to RAISE the ceiling, and letting an operator
// lower it below the stock 2GiB would only manufacture the read-only outage it exists to prevent.
const (
	etcdMinQuotaBytes int64 = 2 * 1024 * 1024 * 1024
	etcdMaxQuotaBytes int64 = 8 * 1024 * 1024 * 1024
)

// etcdQuotaBytes is the --quota-backend-bytes baked into every cluster's etcd (KAAS_ETCD_QUOTA_BYTES,
// default 8GiB). Clamped rather than rejected, in the forgiving style of the other tunables: a bad
// value here would otherwise surface as a crash-looping etcd on the next cluster created, which is a
// long way from the typo that caused it.
func etcdQuotaBytes() int64 {
	n := int64(envInt("KAAS_ETCD_QUOTA_BYTES", int(etcdMaxQuotaBytes)))
	return min(max(n, etcdMinQuotaBytes), etcdMaxQuotaBytes)
}

// etcdPolicy is the reconciler's EtcdPolicy (automatic etcd maintenance) from env.
// KAAS_ETCD_MAINTENANCE gates the whole feature (default on); the rest tune when defragmentation is
// worth its stop-the-world cost. Disabled returns the zero policy, which turns off observation too -
// the reconciler then never reads a cluster's etcd at all.
//
// The window is deliberately shared with anything else that may later need one: a defrag is the
// first disruptive periodic operation the platform performs on a running cluster, but it won't be
// the last. An unparseable window is treated as "no window" (always allowed) rather than failing the
// boot - same forgiving style as envDuration - but it is logged, because silently maintaining
// clusters at 14:00 is a surprise worth one line in the log.
func etcdPolicy(log *slog.Logger) domain.EtcdDefragPolicy {
	if !envBool("KAAS_ETCD_MAINTENANCE", true) {
		return domain.EtcdDefragPolicy{}
	}
	window, err := domain.ParseMaintenanceWindow(os.Getenv("KAAS_MAINTENANCE_WINDOW"), os.Getenv("KAAS_MAINTENANCE_TZ"))
	if err != nil {
		log.Warn("ignoring KAAS_MAINTENANCE_WINDOW", "err", err)
		window = domain.MaintenanceWindow{}
	}
	return domain.EtcdDefragPolicy{
		Enabled:         true,
		ObserveInterval: envDuration("KAAS_ETCD_OBSERVE_INTERVAL", 6*time.Hour),
		MinRatio:        envFloat("KAAS_ETCD_DEFRAG_RATIO", 0.45),
		MinBytes:        int64(envInt("KAAS_ETCD_DEFRAG_MIN_BYTES", 100*1024*1024)),
		MinInterval:     envDuration("KAAS_ETCD_DEFRAG_MIN_INTERVAL", 24*time.Hour),
		Window:          window,
	}
}

// snapshotPolicy is the reconciler's SnapshotPolicy (periodic control-plane backups) from env.
// KAAS_ETCD_SNAPSHOT gates the whole feature (default ON - a platform responsible for keeping
// clusters operational cannot make its only recovery path opt-in), and the rest tune cadence,
// retention, and how stale a backup may be and still be restored automatically.
//
// The interval is the number that matters: it is simultaneously how often a backup is taken and the
// BOUND ON HOW MUCH AN AUTOMATIC RESTORE LOSES. Six hours is the demo-scale trade - frequent enough
// that a recovery is not absurd, infrequent enough that a fleet of clusters is not constantly
// streaming multi-megabyte archives into one Postgres.
func snapshotPolicy() domain.EtcdSnapshotPolicy {
	if !envBool("KAAS_ETCD_SNAPSHOT", true) {
		return domain.EtcdSnapshotPolicy{}
	}
	return domain.EtcdSnapshotPolicy{
		Enabled:  true,
		Interval: envDuration("KAAS_ETCD_SNAPSHOT_INTERVAL", 6*time.Hour),
		Retain:   envInt("KAAS_ETCD_SNAPSHOT_RETAIN", 3),
		// A day. Past it the platform refuses to restore on its own: putting back a keyspace older
		// than that is not obviously better than the outage it replaces - every object created since
		// would vanish, including ones other systems believe exist - and it is a decision a human
		// should make, not a control loop.
		MaxRestoreAge: envDuration("KAAS_ETCD_SNAPSHOT_MAX_RESTORE_AGE", 24*time.Hour),
	}
}

// repairPolicy is the reconciler's RepairPolicy (automatic cluster and node repair) from env.
//
// Default ON, including the destructive rung, because the platform's job is to keep clusters
// operational and a node that stays broken until someone notices is the failure this exists to
// prevent. The guards - not the switch - are what make that safe, and every one of them is tunable
// here precisely because a deployment that wants to be more conservative should tighten a threshold
// rather than turn the feature off.
//
// The two blast-radius fractions deserve their defaults explained. MaxUnhealthyFraction (0.5) is
// Cluster API's MachineHealthCheck reasoning: past half the nodes, this is one cluster-wide fault
// wearing N masks and rebuilding nodes makes it worse. MaxUnhealthyClusters (0.5) is the same guard
// across the FLEET, and it is the one no per-cluster check can replace - when the worker loses the
// hypervisor or the tunnel, every cluster goes unhealthy at once and each looks locally repairable.
func repairPolicy() domain.RepairPolicy {
	if !envBool("KAAS_REPAIR", true) {
		return domain.RepairPolicy{}
	}
	return domain.RepairPolicy{
		Enabled: true,
		// Each destructive rung has its own gate, so an operator can keep the cheap repairs
		// (power-on, rejoin, kubelet restart) while forbidding the platform to rebuild a node or
		// roll a cluster back to a snapshot unattended.
		Replace:         envBool("KAAS_REPAIR_REPLACE", true),
		Restore:         envBool("KAAS_REPAIR_RESTORE", true),
		ObserveInterval: envDuration("KAAS_REPAIR_INTERVAL", 2*time.Minute),
		// Three health sweeps (healthInterval is 20s). Past that the snapshot is not "nothing is
		// wrong", it is "nobody has looked recently" - and acting on a stale reading is how a
		// platform repairs a fault that was fixed twenty minutes ago.
		HealthMaxAge: envDuration("KAAS_REPAIR_HEALTH_MAX_AGE", time.Minute),
		// Kubernetes marks a node NotReady 40s after its kubelet stops reporting, and kubelets miss
		// heartbeats for entirely transient reasons. Ten minutes is long enough that the node was
		// genuinely going to stay down.
		NotReadyGrace: envDuration("KAAS_REPAIR_NOTREADY_GRACE", 10*time.Minute),
		// Cluster API's nodeStartupTimeout, and the guard that keeps repair from fighting
		// provisioning: every freshly created node has no node object for the minutes between its VM
		// booting and kubeadm join returning.
		NodeStartupGrace:     envDuration("KAAS_REPAIR_STARTUP_GRACE", 20*time.Minute),
		ReplaceAfter:         envDuration("KAAS_REPAIR_REPLACE_AFTER", 30*time.Minute),
		MaxUnhealthyFraction: envFloat("KAAS_REPAIR_MAX_UNHEALTHY", 0.5),
		MaxUnhealthyClusters: envFloat("KAAS_REPAIR_MAX_UNHEALTHY_CLUSTERS", 0.5),
		MaxAttempts:          envInt("KAAS_REPAIR_MAX_ATTEMPTS", 3),
		Backoff:              envDuration("KAAS_REPAIR_BACKOFF", 30*time.Minute),
	}
}

// workDir is the base for per-cluster OpenTofu/Ansible workspaces.
func workDir() string {
	return getenv("KAAS_WORK_DIR", filepath.Join(os.TempDir(), "kaas-workspaces"))
}

// reconcileJobTimeout is River's per-job SIGKILL budget for a single reconcile phase (see
// river.go). 15m covers the slowest LOCAL step (a `helm --wait` install); it does NOT size for
// EnsureNodes uploading a cluster's base image to a remote KVM host over the KAAS_KVM_HOST SSH
// tunnel - that transfer can legitimately take much longer on a slow link, and a timeout shorter
// than it means the job is killed and retried before the upload ever finishes, forever. There is no
// good default for "how slow is your link to the hypervisor", so this is a knob, not a constant.
func reconcileJobTimeout(log *slog.Logger) time.Duration {
	raw := getenv("KAAS_RECONCILE_JOB_TIMEOUT", "15m")
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Warn("invalid KAAS_RECONCILE_JOB_TIMEOUT, using 15m", "value", raw, "err", err)
		return 15 * time.Minute
	}
	return d
}

// buildProvisioners builds one provisioner per ENABLED infrastructure provider
// (KAAS_INFRA_PROVIDERS), each selected by KAAS_PROVISIONER (fake|tofu) - the two axes are
// orthogonal: the first says which infrastructures a user may pick from, the second whether they
// are real. In fake mode every provider shares ONE fake provisioner, so the whole multi-provider
// flow (wizard step, provider badge, reconcile, GC) demos without KVM or vCenter; the reconciler
// dedupes it by identity when sweeping (see reconcile.provisioners).
func buildProvisioners(log *slog.Logger, sink events.Sink, kvm *kvmhost.Host, providers []string) (map[string]provision.Provisioner, error) {
	mode := strings.ToLower(getenv("KAAS_PROVISIONER", "fake"))
	out := make(map[string]provision.Provisioner, len(providers))
	if mode == "fake" {
		fake := provision.NewFake()
		for _, p := range providers {
			out[p] = fake
		}
		return out, nil
	}
	if mode != "tofu" {
		return nil, fmt.Errorf("unknown KAAS_PROVISIONER %q (want fake|tofu)", os.Getenv("KAAS_PROVISIONER"))
	}
	for _, p := range providers {
		var (
			prov provision.Provisioner
			err  error
		)
		switch p {
		case domain.ProviderKVM:
			prov, err = buildTofuProvisioner(log, sink, kvm)
		case domain.ProviderVSphere:
			prov, err = buildVSphereProvisioner(log, sink)
		case domain.ProviderProxmox:
			prov, err = buildProxmoxProvisioner(log, sink)
		default:
			err = fmt.Errorf("no real provisioner for infrastructure provider %q", p)
		}
		if err != nil {
			return nil, err
		}
		out[p] = prov
	}
	return out, nil
}

// buildTofuProvisioner is the real KVM path: OpenTofu against the libvirt module. Requires
// KAAS_BASE_IMAGE and KAAS_SSH_PUBLIC_KEY.
func buildTofuProvisioner(log *slog.Logger, sink events.Sink, kvm *kvmhost.Host) (provision.Provisioner, error) {
	moduleDir, err := filepath.Abs(getenv("KAAS_LIBVIRT_MODULE_DIR", "infra/libvirt"))
	if err != nil {
		return nil, err
	}
	return tofu.New(tofu.Config{
		Bin:       getenv("KAAS_TOFU_BIN", "tofu"),
		ModuleDir: moduleDir,
		WorkDir:   workDir(),
		// qemu:///system locally, qemu+ssh://... for a remote KVM host (KAAS_LIBVIRT_URI still wins).
		LibvirtURI: kvm.LibvirtURI(),
		Pool:       getenv("KAAS_LIBVIRT_POOL", "default"),
		// Remote only: pre-stage golden images in the hypervisor's pool and back node volumes onto
		// them there, instead of streaming each through OpenTofu once per cluster. nil locally, where
		// the provider does the import itself.
		Stager:       imageStager(kvm),
		BaseImage:    os.Getenv("KAAS_BASE_IMAGE"),
		ImageDir:     os.Getenv("KAAS_IMAGE_DIR"),
		SSHPublicKey: os.Getenv("KAAS_SSH_PUBLIC_KEY"),
		Events:       sink,
		Log:          log,
	})
}

// buildVSphereProvisioner is the real vSphere path: OpenTofu against the vSphere module, optionally
// decorated with NetBox IPAM registration (see maybeWrapNetbox). Its workspaces live under a
// subdirectory of KAAS_WORK_DIR so the backends' orphan sweeps can never see each other's clusters.
func buildVSphereProvisioner(log *slog.Logger, sink events.Sink) (provision.Provisioner, error) {
	moduleDir, err := filepath.Abs(getenv("KAAS_VSPHERE_MODULE_DIR", "infra/vsphere"))
	if err != nil {
		return nil, err
	}
	prov, err := vsphere.New(vsphere.Config{
		Bin:            getenv("KAAS_TOFU_BIN", "tofu"),
		ModuleDir:      moduleDir,
		WorkDir:        filepath.Join(workDir(), "vsphere"),
		URL:            os.Getenv("KAAS_VSPHERE_URL"),
		Username:       os.Getenv("KAAS_VSPHERE_USERNAME"),
		Password:       os.Getenv("KAAS_VSPHERE_PASSWORD"),
		Insecure:       os.Getenv("KAAS_VSPHERE_INSECURE") == "1",
		Datacenter:     os.Getenv("KAAS_VSPHERE_DATACENTER"),
		ComputeCluster: os.Getenv("KAAS_VSPHERE_CLUSTER"),
		Datastore:      os.Getenv("KAAS_VSPHERE_DATASTORE"),
		ParentFolder:   os.Getenv("KAAS_VSPHERE_FOLDER"),
		SSHPublicKey:   os.Getenv("KAAS_SSH_PUBLIC_KEY"),
		Events:         sink,
		Log:            log,
	})
	if err != nil {
		return nil, err
	}
	return maybeWrapNetbox(prov, domain.ProviderVSphere, log, sink)
}

// buildProxmoxProvisioner is the real Proxmox path: OpenTofu against the Proxmox module, optionally
// decorated with NetBox IPAM registration (see maybeWrapNetbox) - like vSphere, a Proxmox cluster
// sits on the operator's SHARED subnet where an external IPAM is worth keeping in step. Its
// workspaces live under their own subdirectory of KAAS_WORK_DIR so the backends' orphan sweeps can
// never see each other's clusters. Auth is a token (KAAS_PROXMOX_API_TOKEN) or a username/password
// (KAAS_PROXMOX_USERNAME/PASSWORD); the provisioner requires exactly one.
func buildProxmoxProvisioner(log *slog.Logger, sink events.Sink) (provision.Provisioner, error) {
	moduleDir, err := filepath.Abs(getenv("KAAS_PROXMOX_MODULE_DIR", "infra/proxmox"))
	if err != nil {
		return nil, err
	}
	prov, err := proxmox.New(proxmox.Config{
		Bin:          getenv("KAAS_TOFU_BIN", "tofu"),
		ModuleDir:    moduleDir,
		WorkDir:      filepath.Join(workDir(), "proxmox"),
		Endpoint:     os.Getenv("KAAS_PROXMOX_ENDPOINT"),
		Insecure:     os.Getenv("KAAS_PROXMOX_INSECURE") == "1",
		APIToken:     os.Getenv("KAAS_PROXMOX_API_TOKEN"),
		Username:     os.Getenv("KAAS_PROXMOX_USERNAME"),
		Password:     os.Getenv("KAAS_PROXMOX_PASSWORD"),
		Node:         os.Getenv("KAAS_PROXMOX_NODE"),
		Datastore:    os.Getenv("KAAS_PROXMOX_DATASTORE"),
		Bridge:       os.Getenv("KAAS_PROXMOX_NET_BRIDGE"),
		VLAN:         envInt("KAAS_PROXMOX_NET_VLAN", 0),
		SSHPublicKey: os.Getenv("KAAS_SSH_PUBLIC_KEY"),
		Events:       sink,
		Log:          log,
	})
	if err != nil {
		return nil, err
	}
	return maybeWrapNetbox(prov, domain.ProviderProxmox, log, sink)
}

// maybeWrapNetbox decorates a shared-network provisioner with NetBox IPAM registration when
// KAAS_NETBOX_URL is set (opt-in), and returns it unchanged otherwise. It is only ever wrapped
// around a shared-subnet backend (vSphere, Proxmox): a KVM cluster's private per-cluster network is
// nobody else's business, so the KVM path never calls this.
func maybeWrapNetbox(prov provision.Provisioner, provider string, log *slog.Logger, sink events.Sink) (provision.Provisioner, error) {
	if os.Getenv("KAAS_NETBOX_URL") == "" {
		return prov, nil
	}
	nb, err := netbox.New(netbox.Config{
		BaseURL:  os.Getenv("KAAS_NETBOX_URL"),
		Token:    os.Getenv("KAAS_NETBOX_TOKEN"),
		Username: os.Getenv("KAAS_NETBOX_USERNAME"),
		Password: os.Getenv("KAAS_NETBOX_PASSWORD"),
		Insecure: os.Getenv("KAAS_NETBOX_INSECURE") == "1",
		Tag:      getenv("KAAS_NETBOX_TAG", netbox.DefaultTag),
		Log:      log,
	})
	if err != nil {
		return nil, err
	}
	log.Info("netbox IPAM registration enabled", "provider", provider, "url", os.Getenv("KAAS_NETBOX_URL"))
	return netbox.Wrap(prov, nb, sink, log), nil
}

// imageStager returns the golden-image stager for a REMOTE KVM host, and nil for a local one (where
// OpenTofu imports images itself). Returning the interface explicitly as nil - rather than handing
// back a nil *kvmhost.Host - matters: a typed nil in an interface is not == nil, so the provisioner
// would take the remote path and try to stage images over an SSH host that isn't configured.
func imageStager(kvm *kvmhost.Host) tofu.ImageStager {
	if !kvm.Remote() {
		return nil
	}
	return kvm
}

// buildConfigManager selects the config manager from KAAS_CONFIG (fake|ansible). Ansible is
// the real path and requires KAAS_SSH_PRIVATE_KEY_FILE.
func buildConfigManager(log *slog.Logger, sink events.Sink, kvm *kvmhost.Host) (config.Manager, error) {
	switch strings.ToLower(getenv("KAAS_CONFIG", "fake")) {
	case "fake":
		return config.NewFake(), nil
	case "ansible":
		dir, err := filepath.Abs(getenv("KAAS_ANSIBLE_DIR", "ansible"))
		if err != nil {
			return nil, err
		}
		return ansible.New(ansible.Config{
			Bin:               getenv("KAAS_ANSIBLE_BIN", "ansible-playbook"),
			PlaybookDir:       dir,
			WorkDir:           workDir(),
			SSHUser:           getenv("KAAS_SSH_USER", "kaas"),
			SSHPrivateKeyFile: os.Getenv("KAAS_SSH_PRIVATE_KEY_FILE"),
			// Empty locally (VMs are directly routable); a ProxyCommand through the KVM host when not.
			SSHCommonArgs: kvm.AnsibleSSHCommonArgs(),
			Events:        sink,
			Log:           log,
			// etcd backend tuning, baked into every cluster at kubeadm init/join and converged onto
			// older members during defragmentation. Deployment-level, not per cluster.
			EtcdQuotaBytes:          etcdQuotaBytes(),
			EtcdCompactionRetention: getenv("KAAS_ETCD_COMPACTION_RETENTION", "1h"),
		})
	default:
		return nil, fmt.Errorf("unknown KAAS_CONFIG %q (want fake|ansible)", os.Getenv("KAAS_CONFIG"))
	}
}

// buildAddonManager selects the add-on installer from KAAS_ADDONS (fake|helm). Helm is the
// real path (installs via `helm upgrade --install` using catalog chart/repo/version).
func buildAddonManager(log *slog.Logger, sink events.Sink, cat *catalog.Catalog, kvm *kvmhost.Host, dnsSettings dns.Settings) (addons.Manager, error) {
	switch strings.ToLower(getenv("KAAS_ADDONS", "fake")) {
	case "fake":
		return addons.NewFake(), nil
	case "helm":
		return helm.New(helm.Config{
			Bin:          getenv("KAAS_HELM_BIN", "helm"),
			KubectlBin:   getenv("KAAS_KUBECTL_BIN", "kubectl"),
			Catalog:      cat,
			WorkDir:      workDir(),
			KubeProxyURL: kvm.KubeProxyURL(),
			Events:       sink,
			Log:          log,
			// Per-cluster facts the catalog cannot carry: external-dns needs the site's DNS server,
			// credential and this cluster's own domain (internal/app/dns.go); longhorn needs a replica
			// count derived from this cluster's worker count (internal/app/storage.go).
			Extras: chainExtras(dnsAddonExtras(dnsSettings), longhornAddonExtras()),
		})
	default:
		return nil, fmt.Errorf("unknown KAAS_ADDONS %q (want fake|helm)", os.Getenv("KAAS_ADDONS"))
	}
}

// buildAddonValuesProvider selects the source of an add-on's chart values.yaml for the in-browser
// editor from KAAS_ADDON_VALUES (auto|helm|fake). `helm show values` reads the chart repo (no cluster
// needed), so unlike the kubectl-proxied seams it runs API-side. Production would proxy it through the
// worker like the others and mint a scoped token; a chart-values lookup touches no cluster state, so
// API-side is a fair shortcut.
//
//   - auto (default): use real helm when a helm binary is on PATH, falling back to the synthesized
//     doc if the fetch fails (offline/repo down) - so the editor shows the full chart values.yaml
//     wherever helm exists, and still works where it doesn't.
//   - helm: force real helm (still falls back to synthesized on error).
//   - fake: always synthesized (hermetic; no helm, no network).
func buildAddonValuesProvider() (values.Provider, error) {
	bin := getenv("KAAS_HELM_BIN", "helm")
	helmWithFallback := values.Fallback{Primary: values.NewHelm(bin), Backup: values.NewFake()}
	switch strings.ToLower(getenv("KAAS_ADDON_VALUES", "auto")) {
	case "auto":
		if _, err := exec.LookPath(bin); err == nil {
			return helmWithFallback, nil
		}
		return values.NewFake(), nil
	case "helm":
		return helmWithFallback, nil
	case "fake":
		return values.NewFake(), nil
	default:
		return nil, fmt.Errorf("unknown KAAS_ADDON_VALUES %q (want auto|helm|fake)", os.Getenv("KAAS_ADDON_VALUES"))
	}
}

// buildMetricsCollector selects the resource-usage telemetry collector from KAAS_METRICS
// (fake|kubectl). kubectl is the real path: it reads the in-cluster metrics API with
// `kubectl get --raw` using the cluster's admin kubeconfig.
func buildMetricsCollector(log *slog.Logger, kvm *kvmhost.Host) (metrics.Collector, error) {
	switch strings.ToLower(getenv("KAAS_METRICS", "fake")) {
	case "fake":
		return metrics.NewFake(), nil
	case "kubectl":
		return kubectl.New(kubectl.Config{
			Bin:          getenv("KAAS_KUBECTL_BIN", "kubectl"),
			WorkDir:      workDir(),
			KubeProxyURL: kvm.KubeProxyURL(),
			Log:          log,
		})
	default:
		return nil, fmt.Errorf("unknown KAAS_METRICS %q (want fake|kubectl)", os.Getenv("KAAS_METRICS"))
	}
}

// buildHealthChecker selects the cluster-health checker from KAAS_HEALTH (fake|kubectl). kubectl is
// the real path: it evaluates the dedicated checks against the cluster API with `kubectl get --raw`
// using the cluster's admin kubeconfig.
func buildHealthChecker(log *slog.Logger, kvm *kvmhost.Host) (health.Checker, error) {
	switch strings.ToLower(getenv("KAAS_HEALTH", "fake")) {
	case "fake":
		return health.NewFake(), nil
	case "kubectl":
		return healthkubectl.New(healthkubectl.Config{
			Bin:          getenv("KAAS_KUBECTL_BIN", "kubectl"),
			WorkDir:      workDir(),
			KubeProxyURL: kvm.KubeProxyURL(),
			Log:          log,
		})
	default:
		return nil, fmt.Errorf("unknown KAAS_HEALTH %q (want fake|kubectl)", os.Getenv("KAAS_HEALTH"))
	}
}

// buildShellBackend selects the in-browser cluster terminal backend from KAAS_SHELL (fake|worker).
// fake is an in-process pseudo-terminal that synthesizes kubectl output from control-plane state
// (keeps make up-fake demoable). worker is the API-side proxy: it can't reach clusters itself, so it
// bridges each session to the host-networked worker's exec agent (see docs/networking.md).
func buildShellBackend(log *slog.Logger) (shell.Backend, error) {
	switch strings.ToLower(getenv("KAAS_SHELL", "fake")) {
	case "fake":
		return shell.NewFake(), nil
	case "worker":
		return proxy.New(execAgents(), shellToken(log), log), nil
	default:
		return nil, fmt.Errorf("unknown KAAS_SHELL %q (want fake|worker)", os.Getenv("KAAS_SHELL"))
	}
}

// buildNodeSSHBackend selects the in-browser node SSH backend from KAAS_NODE_SSH (fake|agent). fake
// is an in-process pseudo-terminal that synthesizes a Linux shell from control-plane state (keeps
// make up-fake demoable). agent is the API-side proxy: it can't reach cluster VMs itself, so it
// bridges each session to the dedicated, host-networked node-ssh sandbox (cmd/node-ssh-agent) - the
// one place that holds the platform's VM SSH key. Note the selector value is "agent", not "worker":
// the backing process is its OWN sandbox, deliberately not the worker (see internal/nodessh).
func buildNodeSSHBackend(log *slog.Logger) (nodessh.Backend, error) {
	switch strings.ToLower(getenv("KAAS_NODE_SSH", "fake")) {
	case "fake":
		return nodessh.NewFake(), nil
	case "agent":
		return nodesshproxy.New(nodeSSHAgents(), nodeSSHToken(log), log), nil
	default:
		return nil, fmt.Errorf("unknown KAAS_NODE_SSH %q (want fake|agent)", os.Getenv("KAAS_NODE_SSH"))
	}
}

// nodeSSHAgents resolves the node-ssh sandbox(es) this API replica forwards SSH sessions to. Like
// KAAS_SHELL_AGENT_ADDR it takes a COMMA-SEPARATED list so the sandbox tier scales out (the sandboxes
// are interchangeable and hold no cross-session state); the pool round-robins and fails over. It is a
// SEPARATE address list from the shell agents because the node-ssh sandbox is a separate container on
// its own port - a single address (the default) needs no configuration.
func nodeSSHAgents() *execagent.Pool {
	return execagent.NewPoolDefault(getenv("KAAS_NODE_SSH_AGENT_ADDR", execagent.DefaultNodeSSHAddr), execagent.DefaultNodeSSHAddr)
}

// nodeSSHToken is the shared secret the API presents to the node-ssh sandbox. It is DISTINCT from
// shellToken (KAAS_NODE_SSH_TOKEN, not KAAS_SHELL_TOKEN) so the two sandboxes are independently
// credentialed - the node-ssh one holds the fleet key, so a leaked shell token must not open it.
// Prefer the explicit token; otherwise derive one from KAAS_SECRET_KEY (both processes share it in
// real mode) with a domain-separated prefix so it never collides with the shell token.
func nodeSSHToken(log *slog.Logger) string {
	if v := os.Getenv("KAAS_NODE_SSH_TOKEN"); v != "" {
		return v
	}
	if v := os.Getenv("KAAS_SECRET_KEY"); v != "" {
		sum := sha256.Sum256([]byte("kaas-node-ssh:" + v))
		return hex.EncodeToString(sum[:])
	}
	log.Warn("KAAS_NODE_SSH_TOKEN and KAAS_SECRET_KEY unset - the node SSH channel is unauthenticated")
	return ""
}

// buildKubeClient selects the Workloads query seam from KAAS_KUBE (fake|worker). fake synthesizes a
// plausible workload set from control-plane state (keeps make up-fake demoable). worker is the
// API-side proxy: like the shell it can't reach clusters itself, so it forwards each kubectl call to
// the host-networked worker's exec agent (reusing KAAS_SHELL_AGENT_ADDR and the shell token).
func buildKubeClient(log *slog.Logger) (kube.Client, error) {
	switch strings.ToLower(getenv("KAAS_KUBE", "fake")) {
	case "fake":
		return kube.NewFake(), nil
	case "worker":
		return kubekubectl.New(kubeproxy.NewExecer(execAgents(), shellToken(log), log)), nil
	default:
		return nil, fmt.Errorf("unknown KAAS_KUBE %q (want fake|worker)", os.Getenv("KAAS_KUBE"))
	}
}

// buildMonitoringQuerier selects the Monitoring page query seam from KAAS_MONITORING (fake|worker).
// fake synthesizes plausible telemetry from control-plane state (keeps make up-fake demoable). worker
// is the API-side proxy: like the Workloads seam it can't reach clusters itself, so it forwards each
// PromQL query (a `kubectl get --raw .../services/proxy/...` invocation) to the host-networked
// worker's exec agent - reusing KAAS_SHELL_AGENT_ADDR and the shell token, exactly like the Kube seam.
func buildMonitoringQuerier(log *slog.Logger) (monitoring.Querier, error) {
	switch strings.ToLower(getenv("KAAS_MONITORING", "fake")) {
	case "fake":
		return monitoring.NewFake(), nil
	case "worker":
		return monitoringpromql.New(kubeproxy.NewExecer(execAgents(), shellToken(log), log)), nil
	default:
		return nil, fmt.Errorf("unknown KAAS_MONITORING %q (want fake|worker)", os.Getenv("KAAS_MONITORING"))
	}
}

// buildSecurityQuerier selects the Security page query seam from KAAS_SECURITY (fake|worker). fake
// synthesizes a plausible set of Trivy reports from control-plane state (keeps make up-fake demoable).
// worker is the API-side proxy: like the Monitoring seam it can't reach clusters itself, so it forwards
// each Trivy CRD read (a `kubectl get <report>.aquasecurity.github.io` invocation) to the
// host-networked worker's exec agent - reusing KAAS_SHELL_AGENT_ADDR and the shell token.
func buildSecurityQuerier(log *slog.Logger) (security.Querier, error) {
	switch strings.ToLower(getenv("KAAS_SECURITY", "fake")) {
	case "fake":
		return security.NewFake(), nil
	case "worker":
		return securitykubectl.New(kubeproxy.NewExecer(execAgents(), shellToken(log), log)), nil
	default:
		return nil, fmt.Errorf("unknown KAAS_SECURITY %q (want fake|worker)", os.Getenv("KAAS_SECURITY"))
	}
}

// buildAuditQuerier selects the Audit tab query seam from KAAS_AUDIT (fake|worker). fake synthesizes a
// plausible, live-drifting stream of API-server audit events from control-plane state (keeps make
// up-fake demoable). worker is the API-side proxy: like the Security/Monitoring seams it can't reach
// clusters itself, so it forwards each read (`kubectl get pods` + `kubectl logs` on the apiserver
// static pod) to the host-networked worker's exec agent - reusing KAAS_SHELL_AGENT_ADDR and the shell
// token, no new transport.
func buildAuditQuerier(log *slog.Logger) (audit.Querier, error) {
	switch strings.ToLower(getenv("KAAS_AUDIT", "fake")) {
	case "fake":
		return audit.NewFake(), nil
	case "worker":
		return auditkubectl.New(kubeproxy.NewExecer(execAgents(), shellToken(log), log)), nil
	default:
		return nil, fmt.Errorf("unknown KAAS_AUDIT %q (want fake|worker)", os.Getenv("KAAS_AUDIT"))
	}
}

// buildTunnel selects the Monitoring page's "Open UI" tunnel seam from KAAS_TUNNEL (fake|worker).
// fake serves a synthesized landing page (keeps make up-fake demoable). worker is the API-side
// reverse proxy: like the other seams it can't reach clusters itself, so it forwards each HTTP
// request to the host-networked worker's exec agent (/http-proxy), which proxies to the in-cluster
// UI through the API server's service proxy - reusing KAAS_SHELL_AGENT_ADDR and the shell token.
func buildTunnel(log *slog.Logger) (tunnel.Proxier, error) {
	switch strings.ToLower(getenv("KAAS_TUNNEL", "fake")) {
	case "fake":
		return tunnel.NewFake(), nil
	case "worker":
		return tunnelproxy.New(execAgents(), shellToken(log), log), nil
	default:
		return nil, fmt.Errorf("unknown KAAS_TUNNEL %q (want fake|worker)", os.Getenv("KAAS_TUNNEL"))
	}
}

// StartShellAgent starts the worker-side exec agent when KAAS_SHELL_LISTEN is set - the host-side
// server the API reaches for real cluster access: bash+kubectl PTY shell sessions (/exec) and the
// Workloads seam's kubectl invocations (/kube-exec, /kube-logs). Non-blocking; a no-op when unset
// (e.g. the API process, which serves the browser side instead). Runs until ctx is cancelled.
func (a *App) StartShellAgent(ctx context.Context) {
	addr := os.Getenv("KAAS_SHELL_LISTEN")
	if addr == "" {
		return
	}
	proxyURL := a.kvm.KubeProxyURL()
	runner := pty.New(getenv("KAAS_SHELL_BIN", "bash"), workDir(), proxyURL, a.Log)
	kubeExec := kubekubectl.NewLocalExecer(getenv("KAAS_KUBECTL_BIN", "kubectl"), workDir(), proxyURL)
	ag := agent.New(shellToken(a.Log), runner, kubeExec, proxyURL, a.Log)
	go func() {
		if err := ag.Serve(ctx, addr); err != nil {
			a.Log.Error("shell exec agent", "err", err)
		}
	}()
}

// execAgents resolves the exec agents this API replica forwards cluster access to (the Terminal's
// PTY and the Workloads/Monitoring/Security kubectl calls all share them). KAAS_SHELL_AGENT_ADDR
// takes a COMMA-SEPARATED list, so the agent tier scales out too: the agents are interchangeable
// (each request carries its own kubeconfig; no session state survives a connection), and the pool
// round-robins over them and fails over. A single address - the default - behaves exactly as before.
func execAgents() *execagent.Pool {
	return execagent.NewPool(getenv("KAAS_SHELL_AGENT_ADDR", execagent.DefaultAddr))
}

// shellToken is the shared secret the API presents to the worker exec agent. Prefer an explicit
// KAAS_SHELL_TOKEN; otherwise derive one from KAAS_SECRET_KEY (both processes share it in real
// mode, so they derive the same token) - mirroring loadKey's env-derived-key shortcut. Empty (and
// unauthenticated) only when neither is set.
func shellToken(log *slog.Logger) string {
	if v := os.Getenv("KAAS_SHELL_TOKEN"); v != "" {
		return v
	}
	if v := os.Getenv("KAAS_SECRET_KEY"); v != "" {
		sum := sha256.Sum256([]byte("kaas-shell-exec:" + v))
		return hex.EncodeToString(sum[:])
	}
	log.Warn("KAAS_SHELL_TOKEN and KAAS_SECRET_KEY unset - the cluster shell exec channel is unauthenticated")
	return ""
}

// buildStore selects persistence: Postgres if DATABASE_URL is set (real, durable),
// otherwise the in-memory store (default; keeps tests and quick runs dependency-free).
func buildStore(log *slog.Logger) (store.Store, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Info("store selected", "backend", "memory")
		return store.NewMemory(), nil
	}
	dir, err := filepath.Abs(getenv("KAAS_MIGRATIONS_DIR", "migrations"))
	if err != nil {
		return nil, err
	}
	log.Info("store selected", "backend", "postgres")
	return postgres.New(context.Background(), dsn, dir)
}

// Close drains the job queue (if any), tears down the KVM host tunnel, and releases store
// resources. Safe to call always.
func (a *App) Close() {
	if a.river != nil {
		a.river.Stop()
	}
	a.kvm.Stop()
	if c, ok := a.Store.(interface{ Close() }); ok {
		c.Close()
	}
}
