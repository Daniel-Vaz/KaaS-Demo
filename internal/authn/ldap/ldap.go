package ldap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/authn"
	goldap "github.com/go-ldap/ldap/v3"
)

// Client authenticates against a real directory.
//
// It holds no connection: every Authenticate dials, does its work and closes. That is a deliberate
// simplification (production would pool), and it is what makes the client trivially safe to use
// from N api replicas concurrently - there is no shared state to race on.
type Client struct {
	cfg *Config
	log *slog.Logger
}

// New builds a client from a validated config.
//
// This is where the bind password is required, rather than in Parse: the config DOCUMENT is equally
// valid without one (the fake directory reads the same file and never binds), so only the thing that
// actually binds gets to insist. Checking at boot rather than at first login turns the single most
// likely misconfiguration into a startup error that names the variable, instead of a generic
// authentication failure at the DC the first time someone tries to sign in.
func New(cfg *Config, log *slog.Logger) (*Client, error) {
	if cfg == nil {
		return nil, errors.New("ldap: nil config")
	}
	if cfg.BindDN != "" && cfg.bindPassword == "" {
		return nil, fmt.Errorf("ldap: bind_dn is set but %s is empty - the service account needs its password", cfg.BindEnvVar)
	}
	log.Info("directory authentication enabled",
		"urls", strings.Join(cfg.URLs, ","),
		"user_base_dn", cfg.UserBaseDN,
		"bind_dn", cfg.BindDN,
		"mappings", len(cfg.Rules))
	return &Client{cfg: cfg, log: log}, nil
}

// Mappings returns the configured rules, for boot-time group seeding.
func (c *Client) Mappings() []authn.Mapping { return c.cfg.Mappings() }

// Authenticate verifies a login against the directory and resolves the user's group claims.
//
// The order of operations is the security-relevant part:
//
//  1. An empty password is rejected outright. An LDAP simple bind with a valid DN and an empty
//     password SUCCEEDS - as an unauthenticated bind, per RFC 4513 §5.1.2. Skipping this check is
//     the classic LDAP auth bypass: every account in the directory would accept a blank password.
//  2. The user is looked up with the SERVICE account first. If no entry matches we return without
//     ever attempting a user bind - an unknown username must not cost the directory a failed-login
//     count, or /auth/login becomes a lockout weapon for names that don't even exist.
//  3. Only then do we bind as the user, on a separate connection, to verify the password.
//  4. Group claims are evaluated afterwards on the service connection.
//
// Every authentication failure collapses to authn.ErrInvalidCredentials. Infrastructure failures
// (no DC reachable, a bad service account) return a real error, because reporting those as a wrong
// password would send users to reset a password that was never wrong.
func (c *Client) Authenticate(ctx context.Context, username, password string) (*authn.Identity, error) {
	if password == "" {
		return nil, authn.ErrInvalidCredentials
	}
	login := strings.TrimSpace(username)
	if login == "" {
		return nil, authn.ErrInvalidCredentials
	}

	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	entry, err := c.findUser(conn, login)
	if err != nil {
		return nil, err
	}

	if err := c.bindAs(ctx, entry.DN, password); err != nil {
		return nil, err
	}

	claims, err := c.claimsFor(conn, entry.DN)
	if err != nil {
		return nil, err
	}

	name := strings.ToLower(strings.TrimSpace(entry.GetAttributeValue(c.cfg.UsernameAttr)))
	if name == "" {
		// The entry authenticated but carries no value for the attribute we key accounts on. That
		// is a config error (wrong username_attr), not a credential problem - surface it, or we'd
		// silently provision an account with an empty username.
		return nil, fmt.Errorf("ldap: %s has no %s attribute - check username_attr", entry.DN, c.cfg.UsernameAttr)
	}
	return &authn.Identity{
		Username:    name,
		DisplayName: entry.GetAttributeValue(c.cfg.DisplayNameAttr),
		Email:       entry.GetAttributeValue(c.cfg.EmailAttr),
		Groups:      claims,
	}, nil
}

// dial connects to the first reachable URL and binds the service account.
func (c *Client) dial(ctx context.Context) (*goldap.Conn, error) {
	conn, err := c.dialUnbound(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.bindService(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func (c *Client) bindService(conn *goldap.Conn) error {
	if c.cfg.BindDN == "" {
		// Anonymous. Most AD deployments refuse this outright; it is here for directories that
		// allow anonymous search, and it is why bind_dn is optional.
		if err := conn.UnauthenticatedBind(""); err != nil {
			return fmt.Errorf("anonymous bind: %w", err)
		}
		return nil
	}
	if err := conn.Bind(c.cfg.BindDN, c.cfg.bindPassword); err != nil {
		// The service account's own failure is an operator problem, never a user's: it must not be
		// reported as a bad user password.
		return fmt.Errorf("bind as %s (check %s): %w", c.cfg.BindDN, c.cfg.BindEnvVar, err)
	}
	return nil
}

// tlsConfig builds the TLS settings for one endpoint. ServerName is pinned to the URL's host so
// certificate verification means something.
func (c *Client) tlsConfig(u *url.URL) (*tls.Config, error) {
	host := u.Hostname()
	cfg := &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: c.cfg.InsecureSkipVerify, //nolint:gosec // opt-in lab shortcut, see config
	}
	if c.cfg.CAFile == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(c.cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("ca_file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("ca_file %s: no certificates found", c.cfg.CAFile)
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// findUser resolves a typed login to exactly one directory entry, using the service connection.
//
// Returns ErrInvalidCredentials for "no such user" - the caller must not be able to tell that apart
// from a wrong password, and crucially must not bind for a name that doesn't exist.
func (c *Client) findUser(conn *goldap.Conn, login string) (*goldap.Entry, error) {
	// EscapeFilter is what stops a login like `*)(objectClass=*` from rewriting the filter into one
	// that matches an arbitrary entry. Never interpolate a login into a filter without it.
	filter := fmt.Sprintf(c.cfg.UserFilter, goldap.EscapeFilter(login))
	req := goldap.NewSearchRequest(
		c.cfg.UserBaseDN,
		goldap.ScopeWholeSubtree, goldap.NeverDerefAliases,
		2, // size limit 2: enough to detect an ambiguous filter, no more
		int(c.cfg.timeoutOrDefault().Seconds()),
		false,
		filter,
		[]string{"dn", c.cfg.UsernameAttr, c.cfg.DisplayNameAttr, c.cfg.EmailAttr},
		nil,
	)
	res, err := conn.Search(req)
	if err != nil {
		// A size-limit-exceeded here means the filter matched >2 - same ambiguity as below.
		if goldap.IsErrorWithCode(err, goldap.LDAPResultSizeLimitExceeded) {
			return nil, fmt.Errorf("ldap: user_filter matches multiple entries for %q - narrow it", login)
		}
		return nil, fmt.Errorf("ldap: user search: %w", err)
	}
	switch len(res.Entries) {
	case 0:
		return nil, authn.ErrInvalidCredentials
	case 1:
		return res.Entries[0], nil
	default:
		// Two entries for one login is a config error, not a credential one. Binding as "whichever
		// came first" would be authenticating an arbitrary person.
		return nil, fmt.Errorf("ldap: user_filter matches %d entries for %q - narrow it", len(res.Entries), login)
	}
}

// bindAs verifies the password by binding as the user, on its OWN connection so the service bind
// on the caller's connection survives.
func (c *Client) bindAs(ctx context.Context, dn, password string) error {
	conn, err := c.dialUnbound(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.Bind(dn, password); err != nil {
		if goldap.IsErrorWithCode(err, goldap.LDAPResultInvalidCredentials) {
			return authn.ErrInvalidCredentials
		}
		// Anything else - account disabled/expired/locked (AD reports these as InvalidCredentials
		// with a sub-code), a server problem - is still not something we can distinguish safely for
		// the user. Log the detail, return the opaque error.
		c.log.Warn("directory user bind failed", "dn", dn, "err", err)
		return authn.ErrInvalidCredentials
	}
	return nil
}

// dialUnbound connects to the first reachable URL, TLS established but no bind yet.
//
// URLs are tried IN ORDER, so the second entry is a failover rather than a load-balanced peer. The
// errors are joined rather than reduced to the last one, because "AD login is broken" is only
// debuggable if you can see why every DC was rejected - a cert failure on dc1 and a timeout on dc2
// are two different mornings.
func (c *Client) dialUnbound(_ context.Context) (*goldap.Conn, error) {
	var errs []error
	for _, raw := range c.cfg.URLs {
		conn, err := c.connect(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", raw, err))
			c.log.Warn("directory unreachable, trying next", "url", raw, "err", err)
			continue
		}
		return conn, nil
	}
	return nil, fmt.Errorf("ldap: no directory reachable: %w", errors.Join(errs...))
}

// connect dials one endpoint and upgrades it if configured. Every operation is bounded by the
// configured timeout: a DC that accepts the connection and then hangs must fail the login, not park
// a request goroutine forever. (go-ldap does not plumb a context through, so the deadline lives on
// the connection rather than on ctx.)
func (c *Client) connect(raw string) (*goldap.Conn, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	tlsCfg, err := c.tlsConfig(u)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: c.cfg.timeoutOrDefault()}
	conn, err := goldap.DialURL(raw, goldap.DialWithDialer(d), goldap.DialWithTLSConfig(tlsCfg))
	if err != nil {
		return nil, err
	}
	conn.SetTimeout(c.cfg.timeoutOrDefault())
	// Upgrade BEFORE any bind - a StartTLS afterwards would be theatre, the password is already on
	// the wire. config.validate() guarantees we only get here unencrypted if insecure: true.
	if u.Scheme == "ldap" && c.cfg.UseStartTLS() {
		if err := conn.StartTLS(tlsCfg); err != nil {
			conn.Close()
			return nil, fmt.Errorf("starttls: %w", err)
		}
	}
	return conn, nil
}

// claimsFor evaluates every mapping rule against the user's own entry.
//
// The trick that makes this general: each rule is a BASE-scoped search on the USER'S OWN DN with
// the rule's filter as the predicate. One entry back means the user matches the rule; zero means
// they don't. So any filter the directory can evaluate becomes a group predicate - including AD's
// LDAP_MATCHING_RULE_IN_CHAIN (1.2.840.113556.1.4.1941) for nested groups, which the DC evaluates
// server-side. No memberOf parsing, no group-tree walking, no transitive-closure logic of our own.
func (c *Client) claimsFor(conn *goldap.Conn, userDN string) ([]authn.GroupClaim, error) {
	var out []authn.GroupClaim
	for _, rule := range c.cfg.Rules {
		req := goldap.NewSearchRequest(
			userDN,
			goldap.ScopeBaseObject, goldap.NeverDerefAliases,
			1, int(c.cfg.timeoutOrDefault().Seconds()), false,
			rule.Filter,
			[]string{"dn"},
			nil,
		)
		res, err := conn.Search(req)
		if err != nil {
			// A malformed filter fails every login, loudly, which is right: silently treating it as
			// "no match" would quietly strip everyone's group access on their next login.
			return nil, fmt.Errorf("ldap: mapping for group %q, filter %q: %w", rule.GroupKey, rule.Filter, err)
		}
		if len(res.Entries) > 0 {
			out = append(out, authn.GroupClaim{GroupKey: rule.GroupKey, Role: rule.Role})
		}
	}
	return out, nil
}
