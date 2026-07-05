// Package nsupdate is the real dns.Registrar: it publishes a cluster's apps wildcard with RFC 2136
// dynamic update by shelling out to `nsupdate` - the same "drive the real tool" shape as the tofu,
// ansible, helm and kubectl seams.
//
// Against the lab's Active Directory domain controllers the zone is set to secure-only dynamic
// updates, so the update is authenticated with GSS-TSIG: `kinit` obtains a ticket for the platform's
// service account into a private credential cache, and `nsupdate -g` uses it. A BIND-style zone with
// a shared TSIG key (-k) and a zone accepting nonsecure updates are both supported too, because the
// only thing that changes is how the packet is signed.
//
// Every update is a delete-then-add of exactly one name, which makes it an idempotent upsert: the
// reconcile loop retries it, and a re-run converges rather than accumulating records.
//
// Shortcuts, in the repo's style: the platform authenticates as ONE service account with write
// access to the whole delegated zone, so the per-cluster domain confinement is a guardrail against
// accident, not a boundary a cluster-admin could not step over (they hold the same credential inside
// their cluster, for external-dns). Production would delegate a zone per cluster and mint a
// per-cluster keytab. And release deletes the platform's own wildcard only: records the in-cluster
// external-dns created under the cluster's domain are left to the site's own hygiene, since
// enumerating them needs a zone transfer we don't ask for. Production would sweep the cluster's
// subtree under the leader lease.
package nsupdate

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/dns"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/events"
	"github.com/Daniel-Vaz/KaaS-demo/internal/procstream"
)

// timeout bounds a single kinit/nsupdate run: a domain controller that is not answering must fail
// the reconcile step (which retries) rather than wedge it.
const timeout = 30 * time.Second

type Config struct {
	Settings    dns.Settings
	NsupdateBin string      // "nsupdate"
	KinitBin    string      // "kinit"
	WorkDir     string      // scratch for the krb5 credential cache / TSIG key file
	Events      events.Sink // optional; streams tool output to the cluster timeline
	Log         *slog.Logger
}

type Registrar struct{ cfg Config }

func New(cfg Config) (*Registrar, error) {
	s, err := cfg.Settings.ValidateUpdate()
	if err != nil {
		return nil, err
	}
	if !s.Enabled() {
		return nil, fmt.Errorf("nsupdate: DNS settings are not configured")
	}
	cfg.Settings = s
	if cfg.NsupdateBin == "" {
		cfg.NsupdateBin = "nsupdate"
	}
	if cfg.KinitBin == "" {
		cfg.KinitBin = "kinit"
	}
	if cfg.WorkDir == "" {
		return nil, fmt.Errorf("nsupdate: WorkDir is required")
	}
	if cfg.Log == nil {
		return nil, fmt.Errorf("nsupdate: Log is required")
	}
	return &Registrar{cfg: cfg}, nil
}

// EnsureCluster upserts "*.<apps domain>. A <LoadBalancerIP>".
func (r *Registrar) EnsureCluster(ctx context.Context, c *domain.Cluster) error {
	if c.AppsDomain == "" {
		return nil // DNS was off when this cluster was admitted; it owns no name
	}
	if c.LoadBalancerIP == "" {
		return fmt.Errorf("dns: cluster %q has no reserved load-balancer address to publish", c.Name)
	}
	if net.ParseIP(c.LoadBalancerIP) == nil {
		return fmt.Errorf("dns: cluster %q has an unparseable load-balancer address %q", c.Name, c.LoadBalancerIP)
	}
	rec := dns.Wildcard(c.AppsDomain)
	// Delete-then-add in one transaction: the record ends up exactly as desired whether it was
	// absent, correct, or pointing somewhere stale.
	script := r.header() + fmt.Sprintf("update delete %s. A\nupdate add %s. %d A %s\nsend\n",
		rec, rec, r.cfg.Settings.TTL, c.LoadBalancerIP)
	if err := r.run(ctx, c, script); err != nil {
		return fmt.Errorf("dns: publish %s: %w", rec, err)
	}
	r.cfg.Log.Info("dns: published", "cluster", c.Name, "record", rec, "a", c.LoadBalancerIP)
	return nil
}

// ReleaseCluster withdraws the wildcard. Deleting an absent record is a successful no-op, so this is
// safe to re-run on every deleting tick.
func (r *Registrar) ReleaseCluster(ctx context.Context, c *domain.Cluster) error {
	if c.AppsDomain == "" {
		return nil
	}
	rec := dns.Wildcard(c.AppsDomain)
	script := r.header() + fmt.Sprintf("update delete %s. A\nsend\n", rec)
	if err := r.run(ctx, c, script); err != nil {
		return fmt.Errorf("dns: withdraw %s: %w", rec, err)
	}
	r.cfg.Log.Info("dns: withdrawn", "cluster", c.Name, "record", rec)
	return nil
}

// header is the preamble every update script shares: which server to talk to, and which zone the
// names belong to. Naming the zone explicitly keeps the update inside our delegation instead of
// letting nsupdate walk up to the parent AD zone's apex.
func (r *Registrar) header() string {
	host, port := r.cfg.Settings.Server, ""
	if h, p, err := net.SplitHostPort(host); err == nil {
		host, port = h, " "+p
	}
	return fmt.Sprintf("server %s%s\nzone %s.\n", host, port, r.cfg.Settings.Zone)
}

// run feeds one update script to nsupdate, authenticated per the configured mode.
func (r *Registrar) run(ctx context.Context, c *domain.Cluster, script string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir := filepath.Join(r.cfg.WorkDir, c.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	env := os.Environ()
	args := []string{}

	switch r.cfg.Settings.Auth {
	case dns.AuthGSS:
		// A credential cache of our own, per cluster: concurrent reconciles must not race over the
		// default one, and it must not leak into anything else running in this container.
		ccache := filepath.Join(dir, "krb5cc")
		krb5conf, err := r.writeKrb5Conf()
		if err != nil {
			return err
		}
		env = append(env, "KRB5CCNAME=FILE:"+ccache, "KRB5_CONFIG="+krb5conf)
		if err := r.kinit(ctx, c, dir, env); err != nil {
			return err
		}
		defer os.Remove(ccache)
		args = append(args, "-g")
	case dns.AuthTSIG:
		keyfile, err := r.writeTSIGKey(dir)
		if err != nil {
			return err
		}
		defer os.Remove(keyfile)
		args = append(args, "-k", keyfile)
	}

	var out strings.Builder
	emit := func(line string) {
		out.WriteString(line + "\n")
		r.emit(c.ID, "info", line)
	}
	stdout, err := procstream.CaptureInput(ctx, dir, env, []byte(script), emit, r.cfg.NsupdateBin, args...)
	if err != nil {
		r.emit(c.ID, "error", fmt.Sprintf("dns update failed: %v - will retry", err))
		return fmt.Errorf("%s: %w: %s", r.cfg.NsupdateBin, err, strings.TrimSpace(out.String()))
	}
	// nsupdate reports a server-side refusal on stdout/stderr and does not always exit non-zero, so
	// a silent failure would otherwise be recorded as a published record.
	if combined := out.String() + string(stdout); strings.Contains(combined, "update failed") {
		return fmt.Errorf("%s: %s", r.cfg.NsupdateBin, strings.TrimSpace(combined))
	}
	return nil
}

// kinit obtains the service account's Kerberos ticket into the private cache named by env's
// KRB5CCNAME. The password goes in on stdin - never on the command line, where it would be readable
// from the process table.
func (r *Registrar) kinit(ctx context.Context, c *domain.Cluster, dir string, env []string) error {
	s := r.cfg.Settings
	principal := s.KrbUsername
	if !strings.Contains(principal, "@") {
		principal += "@" + s.KrbRealm
	}
	var out strings.Builder
	emit := func(line string) { out.WriteString(line + "\n") }
	// MIT kinit reads the password from stdin when stdin is not a terminal.
	if _, err := procstream.CaptureInput(ctx, dir, env, []byte(s.KrbPassword+"\n"), emit, r.cfg.KinitBin, principal); err != nil {
		r.emit(c.ID, "error", "kerberos authentication for the DNS update failed - will retry")
		return fmt.Errorf("kinit %s: %w: %s", principal, err, strings.TrimSpace(out.String()))
	}
	return nil
}

// writeKrb5Conf writes the minimal Kerberos configuration the container needs, rather than relying
// on whatever /etc/krb5.conf the base image's package happened to leave behind (which names no realm
// at all). It pins the platform's realm as the default and leaves KDC discovery to the DNS SRV
// records every AD domain publishes - so a lab needs no KDC address configured anywhere.
func (r *Registrar) writeKrb5Conf() (string, error) {
	path := filepath.Join(r.cfg.WorkDir, "krb5.conf")
	body := fmt.Sprintf("[libdefaults]\n\tdefault_realm = %s\n\tdns_lookup_kdc = true\n\tdns_lookup_realm = true\n\trdns = false\n",
		r.cfg.Settings.KrbRealm)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// writeTSIGKey materializes the shared key as a key file (0600) rather than passing it as
// `nsupdate -y alg:name:secret`, which would put the secret on the command line.
func (r *Registrar) writeTSIGKey(dir string) (string, error) {
	s := r.cfg.Settings
	if _, err := base64.StdEncoding.DecodeString(s.TSIGSecret); err != nil {
		return "", fmt.Errorf("dns: KAAS_DNS_TSIG_SECRET is not base64: %w", err)
	}
	path := filepath.Join(dir, "tsig.key")
	body := fmt.Sprintf("key \"%s\" {\n\talgorithm %s;\n\tsecret \"%s\";\n};\n", s.TSIGKeyName, s.TSIGAlgo, s.TSIGSecret)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func (r *Registrar) emit(clusterID, level, msg string) {
	if r.cfg.Events != nil {
		r.cfg.Events.Emit(events.Event{ClusterID: clusterID, Level: level, Source: "dns", Message: msg})
	}
}
