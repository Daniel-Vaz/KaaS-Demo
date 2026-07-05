// Package winrmdns is a real dns.Registrar for the platform's per-cluster wildcard record, driven
// over WinRM instead of RFC 2136 dynamic update.
//
// Why this exists alongside internal/dns/nsupdate: Windows DNS Server's dynamic-update handler
// REFUSES any update that would create a wildcard resource record ("*.foo A ..."), regardless of
// the zone's dynamic-updates setting or the auth mode - confirmed against a real domain controller:
// a plain A record updates fine over nsupdate, the identical request for a wildcard name comes back
// REFUSED. Since the platform's wildcard is the ONLY record this registrar ever writes (see
// internal/dns's package doc), nsupdate can never work for it against a Windows DC, so this seam
// bypasses RFC 2136 entirely and drives the DNS Server role's own PowerShell module
// (Add-DnsServerResourceRecordA / Remove-DnsServerResourceRecord / Get-DnsServerResourceRecord)
// remotely instead, which has no such restriction. external-dns (the in-cluster add-on) is
// unaffected by any of this: it only ever creates concrete, non-wildcard hostnames, so its own
// RFC 2136 provider keeps using nsupdate/GSS-TSIG unchanged (internal/app/dns.go).
//
// Every update is Get-then-conditional-Remove-then-Add of exactly one name - the same
// idempotent-upsert shape as nsupdate's delete-then-add - and release is a Get-then-conditional-
// Remove alone, so re-running on retry or on every deleting tick converges rather than erroring on
// an absent record.
//
// Shortcut, in the repo's style: authentication defaults to NTLM, which works against a default
// "winrm quickconfig" listener with no extra setup, rather than Kerberos - production would use
// Kerberos (a keytab, the same shape as nsupdate's GSS-TSIG) so the credential sent is bound to the
// intended server rather than an NTLM hash usable against whatever host answers on KAAS_WINRM_HOST.
package winrmdns

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/masterzen/winrm"

	"github.com/Daniel-Vaz/KaaS-demo/internal/dns"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/events"
)

// timeout bounds a single WinRM PowerShell invocation: an unreachable domain controller must fail
// the reconcile step (which retries) rather than wedge it.
const timeout = 30 * time.Second

// psRunner is the slice of *winrm.Client this package needs, narrowed so a fake can stand in for
// tests without a real WinRM endpoint.
type psRunner interface {
	RunPSWithContext(ctx context.Context, command string) (stdout, stderr string, exitCode int, err error)
}

type Config struct {
	// Settings supplies Zone/Server/TTL. Auth/Krb/TSIG fields are unused here - WinRM has its own
	// credential below, orthogonal to nsupdate's.
	Settings dns.Settings

	Host     string // the WinRM endpoint to connect to - often the DC itself
	Port     int    // default 5986 (https) / 5985 (http)
	HTTPS    bool
	Insecure bool // skip TLS verification (a lab's self-signed WinRM cert)
	Username string
	Password string

	Events events.Sink
	Log    *slog.Logger
}

type Registrar struct {
	cfg    Config
	client psRunner
	zone   string
}

func New(cfg Config) (*Registrar, error) {
	s, err := cfg.Settings.Validate()
	if err != nil {
		return nil, err
	}
	if !s.Enabled() {
		return nil, fmt.Errorf("winrmdns: DNS settings are not configured")
	}
	if strings.TrimSpace(s.Server) == "" {
		return nil, fmt.Errorf("winrmdns: KAAS_DNS_SERVER is required (the DNS server to manage)")
	}
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, fmt.Errorf("winrmdns: KAAS_WINRM_HOST is required")
	}
	if strings.TrimSpace(cfg.Username) == "" || cfg.Password == "" {
		return nil, fmt.Errorf("winrmdns: KAAS_WINRM_USERNAME and KAAS_WINRM_PASSWORD are required")
	}
	if cfg.Log == nil {
		return nil, fmt.Errorf("winrmdns: Log is required")
	}
	if cfg.Port == 0 {
		if cfg.HTTPS {
			cfg.Port = 5986
		} else {
			cfg.Port = 5985
		}
	}
	cfg.Settings = s

	endpoint := winrm.NewEndpoint(cfg.Host, cfg.Port, cfg.HTTPS, cfg.Insecure, nil, nil, nil, timeout)
	params := winrm.NewParameters("PT30S", "en-US", 153600)
	params.TransportDecorator = func() winrm.Transporter { return winrm.NewClientNTLMWithDial(nil) }
	client, err := winrm.NewClientWithParameters(endpoint, cfg.Username, cfg.Password, params)
	if err != nil {
		return nil, fmt.Errorf("winrmdns: %w", err)
	}
	return &Registrar{cfg: cfg, client: client, zone: s.Zone}, nil
}

// EnsureCluster upserts "*.<apps domain>. A <LoadBalancerIP>".
func (r *Registrar) EnsureCluster(ctx context.Context, c *domain.Cluster) error {
	if c.AppsDomain == "" {
		return nil // DNS was off when this cluster was admitted; it owns no name
	}
	if c.LoadBalancerIP == "" {
		return fmt.Errorf("dns: cluster %q has no reserved load-balancer address to publish", c.Name)
	}
	rec := dns.Wildcard(c.AppsDomain)
	name, err := relativeName(rec, r.zone)
	if err != nil {
		return err
	}
	if err := r.run(ctx, c.ID, r.ensureScript(name, c.LoadBalancerIP)); err != nil {
		return fmt.Errorf("dns: publish %s: %w", rec, err)
	}
	r.cfg.Log.Info("winrmdns: published", "cluster", c.Name, "record", rec, "a", c.LoadBalancerIP)
	return nil
}

// ReleaseCluster withdraws the wildcard. Deleting an absent record is a successful no-op, so this
// is safe to re-run on every deleting tick.
func (r *Registrar) ReleaseCluster(ctx context.Context, c *domain.Cluster) error {
	if c.AppsDomain == "" {
		return nil
	}
	rec := dns.Wildcard(c.AppsDomain)
	name, err := relativeName(rec, r.zone)
	if err != nil {
		return err
	}
	if err := r.run(ctx, c.ID, r.releaseScript(name)); err != nil {
		return fmt.Errorf("dns: withdraw %s: %w", rec, err)
	}
	r.cfg.Log.Info("winrmdns: withdrawn", "cluster", c.Name, "record", rec)
	return nil
}

// ensureScript deletes any existing record of this name then adds it fresh - the same
// delete-then-add idempotent-upsert shape as nsupdate.EnsureCluster.
//
// Deliberately NO -ComputerName on any of these cmdlets. We WinRM directly onto the DNS server
// (KAAS_WINRM_HOST defaults to KAAS_DNS_SERVER), so the script already runs in a PowerShell session
// on that machine - omitting -ComputerName makes the DnsServer module operate on the local machine
// via a local, non-networked call. Passing it anyway would make the cmdlet open a SECOND, genuinely
// networked CIM connection back to the same box, which is the classic PowerShell "double hop":
// WinRM's default auth (NTLM here) does not delegate the caller's credential to that second hop, so
// the CIM connection is rejected with "Access is denied" even though the account has every DNS
// right it needs - confirmed against a real DC. There is no in-scope fix that keeps -ComputerName
// (that needs CredSSP or constrained delegation), so this seam only ever manages the DNS server it
// is directly connected to.
func (r *Registrar) ensureScript(name, ip string) string {
	zone, n, ipq := psQuote(r.zone), psQuote(name), psQuote(ip)
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$existing = Get-DnsServerResourceRecord -ZoneName %[1]s -Name %[2]s -RRType A -ErrorAction SilentlyContinue
if ($existing) { $existing | Remove-DnsServerResourceRecord -ZoneName %[1]s -Force }
Add-DnsServerResourceRecordA -ZoneName %[1]s -Name %[2]s -IPv4Address %[3]s -TimeToLive (New-TimeSpan -Seconds %[4]d)
exit 0
`, zone, n, ipq, r.cfg.Settings.TTL)
}

// releaseScript is the same no -ComputerName reasoning as ensureScript, delete-only.
//
// The trailing `exit 0` is load-bearing: withdrawing an ABSENT record is a successful no-op (the
// common case - a cluster deleted before its wildcard was ever published, dns_wired=f), but without
// it the script's last executed statement is the Get, which for a missing name raises a not-found
// error that -ErrorAction SilentlyContinue SUPPRESSES yet still leaves $?=$false, so PowerShell's
// process exits 1 with empty stderr and the delete step retries forever. `exit 0` is reached only
// when no terminating error fired ($ErrorActionPreference='Stop'), so a genuine Remove failure still
// throws and surfaces a non-zero exit before it.
func (r *Registrar) releaseScript(name string) string {
	zone, n := psQuote(r.zone), psQuote(name)
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$existing = Get-DnsServerResourceRecord -ZoneName %[1]s -Name %[2]s -RRType A -ErrorAction SilentlyContinue
if ($existing) { $existing | Remove-DnsServerResourceRecord -ZoneName %[1]s -Force }
exit 0
`, zone, n)
}

func (r *Registrar) run(ctx context.Context, clusterID, script string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout, stderr, exitCode, err := r.client.RunPSWithContext(ctx, script)
	if trimmed := strings.TrimSpace(stdout); trimmed != "" {
		r.emit(clusterID, "info", trimmed)
	}
	if err != nil {
		r.emit(clusterID, "error", fmt.Sprintf("winrm command failed: %v - will retry", err))
		return err
	}
	if exitCode != 0 {
		msg := strings.TrimSpace(stderr)
		r.emit(clusterID, "error", fmt.Sprintf("dns update failed (exit %d): %s - will retry", exitCode, msg))
		return fmt.Errorf("exit status %d: %s", exitCode, msg)
	}
	return nil
}

func (r *Registrar) emit(clusterID, level, msg string) {
	if r.cfg.Events != nil {
		r.cfg.Events.Emit(events.Event{ClusterID: clusterID, Level: level, Source: "dns", Message: msg})
	}
}

// psQuote wraps a string as a PowerShell single-quoted literal, doubling any embedded single quote
// - the correct escaping for that quoting style. (Every value passed through this in practice is
// already validated upstream - a DNS-1123 cluster/pool name, an IP, or operator config - so this
// exists as a defensive backstop, not a load-bearing sanitizer.)
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// relativeName strips the trailing ".<zone>" from a fully-qualified record name, since the
// DnsServer PowerShell cmdlets take a name relative to -ZoneName rather than a FQDN.
func relativeName(fqdn, zone string) (string, error) {
	suffix := "." + strings.Trim(zone, ".")
	if !strings.HasSuffix(fqdn, suffix) {
		return "", fmt.Errorf("winrmdns: record %q is not inside zone %q", fqdn, zone)
	}
	return strings.TrimSuffix(fqdn, suffix), nil
}
