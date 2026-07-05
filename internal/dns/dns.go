// Package dns is the DNS seam: it publishes each cluster's platform-owned wildcard record into the
// site's DNS (a delegated zone on the lab's Active Directory domain controllers) and releases it
// when the cluster is destroyed.
//
// Only ONE record is platform-owned: the per-cluster apps wildcard
//
//	*.apps.<cluster>.kaas.<base domain>.  A  <cluster.LoadBalancerIP>
//
// pointing at the address the default Envoy Gateway draws from the cluster's MetalLB pool (see
// the default_gateway ansible role). That is what makes "deploy an app, expose it on
// <anything>.apps.<cluster>.kaas.<domain>, routing just works" true the moment a cluster goes Ready,
// with nothing for the user to configure.
//
// Why the PLATFORM owns it rather than the in-cluster external-dns add-on: external-dns lives inside
// the cluster, so when the cluster is destroyed nothing is left to delete its records - and
// LoadBalancerIP is recycled to the next cluster on the same subnet, so an orphaned wildcard would
// resolve straight into ANOTHER tenant's gateway. The record's lifecycle is the cluster's lifecycle,
// which only the control plane knows, so the control plane writes it: created at gateway wiring,
// deleted before the infrastructure is torn down. Same shape (and the same "a failure fails the
// reconcile step, because it retries and converges" rule) as the NetBox IPAM decorator.
//
// external-dns is still bundled and configured per cluster - it owns the records a USER's
// Services/Ingresses/HTTPRoutes ask for, inside that cluster's own domain filter. It never touches
// the wildcard: the platform writes no ownership TXT for it, so external-dns's txt registry
// considers it unmanaged.
//
// The real implementation (internal/dns/nsupdate) shells out to `nsupdate` - RFC 2136 dynamic
// update, GSS-TSIG against AD's secure-update policy; Fake just logs. Unset KAAS_DNS_BASE_DOMAIN
// disables the feature entirely and every cluster is created with no domain at all.
package dns

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// Auth modes for the dynamic update (KAAS_DNS_AUTH).
const (
	// AuthGSS is GSS-TSIG (Kerberos): what a Windows DNS zone set to "Secure only" dynamic
	// updates requires, and the lab default.
	AuthGSS = "gss"
	// AuthTSIG is a shared TSIG key - BIND's usual mechanism, and what a non-AD zone would use.
	AuthTSIG = "tsig"
	// AuthNone is an unauthenticated update: only for a zone that accepts nonsecure updates.
	AuthNone = "none"
)

// Settings is the deployment-level DNS configuration: which delegated zone this platform owns, on
// which server, and how it authenticates. It is deployment config, never per cluster - a cluster's
// own names are derived from it once, at admission, and then stored on the cluster row (see
// domain.Cluster.DNSDomain/AppsDomain) so a later edit here can never move an existing cluster's
// domain out from under its users.
type Settings struct {
	// BaseDomain is the zone this platform hands cluster subdomains out of, e.g.
	// "kaas.example.internal". Empty disables the whole feature.
	BaseDomain string
	// AppsLabel is the label between the cluster and its app hostnames: "apps" gives
	// *.apps.<cluster>.<base>. Configurable so a site with a different convention can follow it.
	AppsLabel string
	// Server is the DNS server the dynamic update is sent to (a domain controller), "host" or
	// "host:port".
	Server string
	// Zone is the zone that actually holds the records - the delegated zone (usually the same as
	// BaseDomain). Naming it explicitly keeps the update inside the delegation instead of at the
	// parent AD zone's apex.
	Zone string
	// TTL of the published record, in seconds.
	TTL int

	// Auth selects the update authentication: gss (AD secure updates), tsig, or none.
	Auth string
	// GSS-TSIG (Kerberos) credentials. The service account must be permitted to write the zone.
	KrbUsername string
	KrbPassword string
	KrbRealm    string
	// TSIG key, when Auth is tsig.
	TSIGKeyName string
	TSIGSecret  string
	TSIGAlgo    string // e.g. "hmac-sha256"
}

// Enabled reports whether this deployment publishes cluster DNS at all.
func (s Settings) Enabled() bool { return strings.TrimSpace(s.BaseDomain) != "" }

// ClusterDomain is the subdomain a cluster owns: "<cluster>.kaas.example.internal". It is the
// cluster's whole DNS namespace - the platform's wildcard lives under it, and the in-cluster
// external-dns is confined to it by its domain filter.
func (s Settings) ClusterDomain(cluster string) string {
	return cluster + "." + strings.Trim(s.BaseDomain, ".")
}

// AppsDomain is the domain the wildcard covers: "apps.<cluster>.kaas.example.internal", so a user's
// app is reachable at <anything>.apps.<cluster>.kaas.example.internal.
func (s Settings) AppsDomain(cluster string) string {
	return s.AppsLabel + "." + s.ClusterDomain(cluster)
}

// labelRE is a DNS-1123 label - what a cluster name must be for its domain to be legal.
var labelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Validate checks the NAMING half of the settings and normalizes the defaults. Called once at
// startup by every process: a half-configured DNS integration should refuse to boot rather than
// fail every cluster at admission.
//
// It deliberately does NOT require a server or credentials, because the API process needs the
// naming half alone - it derives a cluster's domains at admission and never writes a record. Only
// the worker builds a Registrar, so only the worker holds the credential that reaches the domain
// controllers (see ValidateUpdate), exactly as it alone holds the hypervisor's.
func (s Settings) Validate() (Settings, error) {
	if !s.Enabled() {
		return Settings{}, nil
	}
	s.BaseDomain = strings.Trim(strings.TrimSpace(strings.ToLower(s.BaseDomain)), ".")
	s.AppsLabel = strings.TrimSpace(strings.ToLower(s.AppsLabel))
	if s.AppsLabel == "" {
		s.AppsLabel = "apps"
	}
	if !labelRE.MatchString(s.AppsLabel) {
		return s, fmt.Errorf("KAAS_DNS_APPS_LABEL %q is not a DNS label", s.AppsLabel)
	}
	for _, l := range strings.Split(s.BaseDomain, ".") {
		if !labelRE.MatchString(l) {
			return s, fmt.Errorf("KAAS_DNS_BASE_DOMAIN %q is not a valid domain", s.BaseDomain)
		}
	}
	if s.Zone == "" {
		s.Zone = s.BaseDomain
	}
	s.Zone = strings.Trim(strings.TrimSpace(strings.ToLower(s.Zone)), ".")
	// The platform can only write names it can reach from the zone it updates: a base domain outside
	// the zone would produce records the server rejects (or, worse, silently drops into the wrong
	// place). Catch it at boot rather than per cluster.
	if s.BaseDomain != s.Zone && !strings.HasSuffix(s.BaseDomain, "."+s.Zone) {
		return s, fmt.Errorf("KAAS_DNS_BASE_DOMAIN %q is not inside KAAS_DNS_ZONE %q", s.BaseDomain, s.Zone)
	}
	if s.TTL <= 0 {
		s.TTL = 300
	}
	return s, nil
}

// ValidateUpdate checks the half of the settings needed to actually WRITE a record: where the
// server is and how the update is signed. Called when a real Registrar is built (the worker) and by
// the add-on wiring, which configures the in-cluster external-dns against the same server.
func (s Settings) ValidateUpdate() (Settings, error) {
	s, err := s.Validate()
	if err != nil || !s.Enabled() {
		return s, err
	}
	if strings.TrimSpace(s.Server) == "" {
		return s, fmt.Errorf("KAAS_DNS_SERVER is required when KAAS_DNS_BASE_DOMAIN is set")
	}
	switch s.Auth = strings.ToLower(strings.TrimSpace(s.Auth)); s.Auth {
	case "":
		s.Auth = AuthGSS
		fallthrough
	case AuthGSS:
		if s.KrbUsername == "" || s.KrbPassword == "" {
			return s, fmt.Errorf("KAAS_DNS_KRB_USERNAME and KAAS_DNS_KRB_PASSWORD are required for GSS-TSIG updates")
		}
		if s.KrbRealm == "" {
			// A user@REALM principal already names it. The realm is NOT derived from the zone: the
			// zone we update is a delegated child (kaas.example.internal) while the realm is the AD
			// domain itself (EXAMPLE.INTERNAL), and guessing would produce tickets for a realm that
			// does not exist.
			if _, realm, ok := strings.Cut(s.KrbUsername, "@"); ok {
				s.KrbRealm = realm
			} else {
				return s, fmt.Errorf("KAAS_DNS_KRB_REALM is required (or give KAAS_DNS_KRB_USERNAME as user@REALM)")
			}
		}
		s.KrbRealm = strings.ToUpper(s.KrbRealm)
	case AuthTSIG:
		if s.TSIGKeyName == "" || s.TSIGSecret == "" {
			return s, fmt.Errorf("KAAS_DNS_TSIG_KEYNAME and KAAS_DNS_TSIG_SECRET are required for TSIG updates")
		}
		if s.TSIGAlgo == "" {
			s.TSIGAlgo = "hmac-sha256"
		}
	case AuthNone:
	default:
		return s, fmt.Errorf("unknown KAAS_DNS_AUTH %q (want gss|tsig|none)", s.Auth)
	}
	return s, nil
}

// AdmitCluster derives a cluster's DNS names at admission. It returns ("", "") when the deployment
// publishes no DNS, so every caller has one branch. The cluster name must be a DNS label - cluster
// names are globally unique (the clusters.name unique constraint), which is exactly what makes
// <cluster>.<base> unique platform-wide with no allocator of our own.
func (s Settings) AdmitCluster(name string) (clusterDomain, appsDomain string, err error) {
	if !s.Enabled() {
		return "", "", nil
	}
	if !labelRE.MatchString(name) {
		return "", "", fmt.Errorf("cluster name %q cannot be published in DNS: it must be lower-case alphanumeric with dashes", name)
	}
	apps := s.AppsDomain(name)
	// "*." + apps must still be a legal name.
	if len(apps)+2 > 253 {
		return "", "", fmt.Errorf("cluster name %q makes the apps domain %q too long", name, apps)
	}
	return s.ClusterDomain(name), apps, nil
}

// Wildcard is the record the platform owns for a cluster: "*.<apps domain>".
func Wildcard(appsDomain string) string { return "*." + appsDomain }

// Registrar publishes and withdraws a cluster's platform-owned DNS. Both operations MUST be
// idempotent: EnsureCluster is a level-triggered upsert re-run on retry, and ReleaseCluster runs on
// every deleting tick until the cluster is gone.
type Registrar interface {
	// EnsureCluster upserts the cluster's apps wildcard onto c.LoadBalancerIP.
	EnsureCluster(ctx context.Context, c *domain.Cluster) error
	// ReleaseCluster withdraws it. Absence is success.
	ReleaseCluster(ctx context.Context, c *domain.Cluster) error
}

// Fake is the default Registrar: it publishes nothing and logs what it would have done, so the
// whole flow (admission, the wiring step, the portal's "apps domain" panel) is demoable under
// `make up-fake` with no domain controller.
type Fake struct{ Log *slog.Logger }

func NewFake(log *slog.Logger) *Fake { return &Fake{Log: log} }

func (f *Fake) EnsureCluster(_ context.Context, c *domain.Cluster) error {
	if f.Log != nil && c.AppsDomain != "" {
		f.Log.Info("dns(fake): publish", "record", Wildcard(c.AppsDomain), "a", c.LoadBalancerIP)
	}
	return nil
}

func (f *Fake) ReleaseCluster(_ context.Context, c *domain.Cluster) error {
	if f.Log != nil && c.AppsDomain != "" {
		f.Log.Info("dns(fake): withdraw", "record", Wildcard(c.AppsDomain))
	}
	return nil
}
