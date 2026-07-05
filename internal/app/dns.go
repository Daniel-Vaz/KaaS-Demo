package app

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/addons"
	"github.com/Daniel-Vaz/KaaS-demo/internal/dns"
	"github.com/Daniel-Vaz/KaaS-demo/internal/dns/nsupdate"
	winrmdns "github.com/Daniel-Vaz/KaaS-demo/internal/dns/winrm"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/events"
)

// externalDNSAddon is the in-cluster add-on this file configures. The PLATFORM's own wildcard is
// published by the dns.Registrar (internal/dns) and not by this add-on - see that package's doc for
// why the two split the way they do.
const externalDNSAddon = "external-dns"

// envoyGatewayAddonName is the add-on that brings the Gateway API CRDs. external-dns only watches
// HTTPRoutes when they exist: pointing its gateway-httproute source at a cluster with no Gateway API
// installed makes it fail its initial sync, so the source is added only when the add-on is present.
const envoyGatewayAddonName = "envoy-gateway"

// dnsCredentialsSecret is the Secret the in-cluster external-dns reads its update credential from.
// It is created in the add-on's namespace before the chart installs (addons.Extras.Secret) rather
// than passed in Helm values: a value would land in the Deployment's env, where any user who can
// read a pod spec in that cluster - including a read-role group-mate - could read the platform's
// directory credential back out.
const dnsCredentialsSecret = "kaas-dns-credentials"

// dnsFromEnv reads the deployment's DNS settings. Unset KAAS_DNS_BASE_DOMAIN disables the whole
// feature: clusters are admitted with no domain, the wiring step never fires, and external-dns (if
// installed) falls back to the catalog's inert inmemory provider.
func dnsFromEnv() (dns.Settings, error) {
	s := dns.Settings{
		BaseDomain:  os.Getenv("KAAS_DNS_BASE_DOMAIN"),
		AppsLabel:   getenv("KAAS_DNS_APPS_LABEL", "apps"),
		Server:      os.Getenv("KAAS_DNS_SERVER"),
		Zone:        os.Getenv("KAAS_DNS_ZONE"),
		TTL:         envInt("KAAS_DNS_TTL", 300),
		Auth:        os.Getenv("KAAS_DNS_AUTH"),
		KrbUsername: os.Getenv("KAAS_DNS_KRB_USERNAME"),
		KrbPassword: os.Getenv("KAAS_DNS_KRB_PASSWORD"),
		KrbRealm:    os.Getenv("KAAS_DNS_KRB_REALM"),
		TSIGKeyName: os.Getenv("KAAS_DNS_TSIG_KEYNAME"),
		TSIGSecret:  os.Getenv("KAAS_DNS_TSIG_SECRET"),
		TSIGAlgo:    os.Getenv("KAAS_DNS_TSIG_ALGO"),
	}
	return s.Validate()
}

// buildDNSRegistrar selects who publishes the platform-owned wildcard, from KAAS_DNS
// (fake|nsupdate|winrm). Only the worker sets a real one, so the credential that can write the
// site's zone stays out of the API container - the same split as the hypervisor and the LDAP bind
// password.
func buildDNSRegistrar(log *slog.Logger, sink events.Sink, s dns.Settings) (dns.Registrar, error) {
	switch strings.ToLower(getenv("KAAS_DNS", "fake")) {
	case "fake", "":
		return dns.NewFake(log), nil
	case "nsupdate", "rfc2136":
		if !s.Enabled() {
			return nil, fmt.Errorf("KAAS_DNS=nsupdate needs KAAS_DNS_BASE_DOMAIN")
		}
		return nsupdate.New(nsupdate.Config{
			Settings:    s,
			NsupdateBin: getenv("KAAS_NSUPDATE_BIN", "nsupdate"),
			KinitBin:    getenv("KAAS_KINIT_BIN", "kinit"),
			WorkDir:     filepath.Join(workDir(), "dns"),
			Events:      sink,
			Log:         log,
		})
	case "winrm":
		// Windows DNS Server refuses to create a wildcard record over RFC 2136 dynamic update no
		// matter how the zone or nsupdate are configured (see internal/dns/winrm's package doc for
		// how that was confirmed) - this is the escape hatch for a Windows DC, driving the DNS
		// Server role's own PowerShell module instead of nsupdate.
		if !s.Enabled() {
			return nil, fmt.Errorf("KAAS_DNS=winrm needs KAAS_DNS_BASE_DOMAIN")
		}
		return winrmdns.New(winrmdns.Config{
			Settings: s,
			Host:     getenv("KAAS_WINRM_HOST", s.Server),
			Port:     envInt("KAAS_WINRM_PORT", 0),
			HTTPS:    !envBool("KAAS_WINRM_INSECURE_HTTP", false),
			Insecure: envBool("KAAS_WINRM_INSECURE", false),
			Username: os.Getenv("KAAS_WINRM_USERNAME"),
			Password: os.Getenv("KAAS_WINRM_PASSWORD"),
			Events:   sink,
			Log:      log,
		})
	default:
		return nil, fmt.Errorf("unknown KAAS_DNS %q (want fake|nsupdate|winrm)", os.Getenv("KAAS_DNS"))
	}
}

// dnsAddonExtras configures the in-cluster external-dns per cluster: which domain it may write
// (its own subdomain and nothing else), which server it writes to, and how it authenticates.
//
// This can't live in the catalog: the DNS server, realm and credential are properties of the
// DEPLOYMENT, and the domain filter is a property of the CLUSTER. The catalog keeps what is true
// everywhere (chart, version, registry, ownership); this supplies the rest, applied last so a user's
// values override cannot unhook their cluster from the platform's DNS.
//
// The tenancy here is honest but not airtight: every cluster's external-dns authenticates as the
// SAME service account, which can write the whole delegated zone, so the per-cluster domain filter
// stops external-dns from straying - not a determined cluster-admin, who holds the credential inside
// their own cluster. Production would delegate a zone per cluster and mint a credential scoped to it.
func dnsAddonExtras(s dns.Settings) addons.ExtrasFunc {
	return func(c *domain.Cluster, a domain.Addon) addons.Extras {
		if a.Name != externalDNSAddon {
			return addons.Extras{}
		}
		// Sources are set here rather than in the catalog because one of them is conditional: the
		// gateway-httproute source needs the Gateway API CRDs, and external-dns fails its initial
		// sync if they are absent - which is exactly the cluster of a user who deselected
		// envoy-gateway.
		values := map[string]string{
			"sources[0]": "service",
			"sources[1]": "ingress",
		}
		if hasAddon(c, envoyGatewayAddonName) {
			values["sources[2]"] = "gateway-httproute"
		}
		if !s.Enabled() || c.DNSDomain == "" {
			// No site DNS (or a cluster admitted before it was configured): leave the catalog's inert
			// inmemory provider in place so the add-on still installs and does nothing.
			return addons.Extras{Values: values}
		}

		host, port := s.Server, ""
		if h, p, err := net.SplitHostPort(s.Server); err == nil {
			host, port = h, p
		}
		values["provider.name"] = "rfc2136"
		// Confined to this cluster's own subdomain, and owning only what it created: the txt registry
		// records ownership per record, so external-dns leaves the platform's wildcard (which carries
		// no ownership TXT) alone even under policy=sync.
		values["domainFilters[0]"] = c.DNSDomain
		values["extraArgs.rfc2136-host"] = host
		values["extraArgs.rfc2136-zone"] = s.Zone
		values["extraArgs.rfc2136-min-ttl"] = strconv.Itoa(s.TTL) + "s"
		if port != "" {
			values["extraArgs.rfc2136-port"] = port
		}

		var secret *addons.SecretSpec
		switch s.Auth {
		case dns.AuthGSS:
			values["extraArgs.rfc2136-gss-tsig"] = "true"
			values["extraArgs.rfc2136-kerberos-realm"] = s.KrbRealm
			values["extraArgs.rfc2136-kerberos-username"] = strings.SplitN(s.KrbUsername, "@", 2)[0]
			secret = &addons.SecretSpec{
				Name:      dnsCredentialsSecret,
				Namespace: externalDNSNamespace,
				Data:      map[string]string{"password": s.KrbPassword},
			}
			// external-dns reads every flag from EXTERNAL_DNS_<FLAG> too, which is how the credential
			// gets in from a Secret instead of from an argument.
			addSecretEnv(values, "EXTERNAL_DNS_RFC2136_KERBEROS_PASSWORD", "password")
		case dns.AuthTSIG:
			values["extraArgs.rfc2136-tsig-keyname"] = s.TSIGKeyName
			values["extraArgs.rfc2136-tsig-secret-alg"] = s.TSIGAlgo
			secret = &addons.SecretSpec{
				Name:      dnsCredentialsSecret,
				Namespace: externalDNSNamespace,
				Data:      map[string]string{"secret": s.TSIGSecret},
			}
			addSecretEnv(values, "EXTERNAL_DNS_RFC2136_TSIG_SECRET", "secret")
		case dns.AuthNone:
			values["extraArgs.rfc2136-insecure"] = "true"
		}
		return addons.Extras{Values: values, Secret: secret}
	}
}

// externalDNSNamespace mirrors the add-on's catalog namespace. The credential Secret must be created
// in it before the chart installs, so it is named here rather than resolved from the catalog at
// install time.
const externalDNSNamespace = "externaldns-system"

// addSecretEnv points one of external-dns's EXTERNAL_DNS_* environment variables at a key of the
// credentials Secret.
func addSecretEnv(values map[string]string, envName, key string) {
	values["env[0].name"] = envName
	values["env[0].valueFrom.secretKeyRef.name"] = dnsCredentialsSecret
	values["env[0].valueFrom.secretKeyRef.key"] = key
}

// hasAddon reports whether the cluster carries an add-on, in any phase - the question is whether the
// chart is part of this cluster's shape, not whether it has finished installing.
func hasAddon(c *domain.Cluster, name string) bool {
	for _, a := range c.Addons {
		if a.Name == name && a.Phase != "removing" {
			return true
		}
	}
	return false
}
