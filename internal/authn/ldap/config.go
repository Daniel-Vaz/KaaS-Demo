// Package ldap authenticates against Active Directory (or any LDAP v3 directory) and maps
// directory attributes onto portal groups and roles.
//
// The deployment-shaped part - which DCs, which service account, where users live, and the rules
// that decide who lands in which group - is a YAML file (KAAS_LDAP_CONFIG), not env vars, because
// the rules are a list of raw LDAP filters and those do not survive a shell variable. The file is
// parsed and validated once at boot, in the spirit of internal/app.vsphereFromEnv: fail fast, with
// a message that names the field.
//
// The bind password is deliberately NOT in the file. The file is config (mountable, reviewable, a
// ConfigMap); the password is a secret and comes from the environment.
package ldap

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/authn"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"gopkg.in/yaml.v3"
)

// Defaults. The user filter and attribute names default to Active Directory's shape, since that is
// what this exists for; an OpenLDAP deployment overrides them (uid / cn / mail).
const (
	defaultUserFilter   = "(&(objectClass=user)(objectCategory=person)(sAMAccountName=%s))"
	defaultUsernameAttr = "sAMAccountName"
	defaultDisplayAttr  = "displayName"
	defaultEmailAttr    = "mail"
	defaultBindEnvVar   = "KAAS_LDAP_BIND_PASSWORD"
	defaultTimeout      = 10 * time.Second
)

// Config is a parsed, validated ldap.yaml.
type Config struct {
	// URLs are the directory endpoints, tried IN ORDER on every authentication - the second is a
	// failover, not a load-balanced peer. ldap:// and ldaps:// both allowed.
	URLs []string `yaml:"urls"`

	// StartTLS upgrades an ldap:// connection to TLS before any bind. Defaults to true, and a
	// plaintext bind requires Insecure - see validate(). Ignored for ldaps:// (already TLS).
	StartTLS *bool `yaml:"start_tls"`
	// CAFile is a PEM bundle for the DC's issuing CA, for the very common case of an internal AD
	// CA the container doesn't trust. Empty uses the system pool.
	CAFile string `yaml:"ca_file"`
	// InsecureSkipVerify disables certificate verification. A lab shortcut; it makes the TLS
	// pointless against an active attacker, who is exactly who TLS is for here.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
	// Insecure is the explicit, deliberate opt-in to binding over plaintext ldap://. Without it a
	// non-TLS config is a boot error rather than a silent credential leak.
	Insecure bool `yaml:"insecure"`

	// BindDN is the service account that searches the directory. Empty means an anonymous bind,
	// which most AD deployments refuse.
	BindDN string `yaml:"bind_dn"`
	// BindEnvVar names the environment variable holding the service account's password. The
	// password itself never appears in this file. Deliberately not named *Password*: this field
	// holds only the NAME of an env var (e.g. "KAAS_LDAP_BIND_PASSWORD"), and error messages that
	// mention it (to tell an operator which var to check) reach a logger - a name containing
	// "password" made static analysis treat this non-secret string as if it were the credential.
	BindEnvVar string `yaml:"bind_password_env"`

	// UserBaseDN is the subtree searched for the login. A whole domain
	// ("DC=example,DC=lab") or a single OU - narrowing this is the cheapest way to say "only
	// these people may log in at all".
	UserBaseDN string `yaml:"user_base_dn"`
	// UserFilter resolves a typed login name to exactly one entry. Must contain exactly one %s,
	// which is replaced with the ESCAPED login. Swap sAMAccountName for userPrincipalName to take
	// UPN logins, or add a clause to exclude disabled accounts.
	UserFilter string `yaml:"user_filter"`
	// UsernameAttr's value becomes the portal username (lowercased) - not what the user typed, so
	// "DVaz", "dvaz" and a UPN all converge on one account.
	UsernameAttr    string `yaml:"username_attr"`
	DisplayNameAttr string `yaml:"display_name_attr"`
	EmailAttr       string `yaml:"email_attr"`

	// Timeout bounds each directory operation ("10s"). A DC that hangs must fail a login, not a
	// request goroutine.
	Timeout string `yaml:"timeout"`

	// Rules decide who lands in which group with which role. Order is irrelevant; a user may match
	// any number of rules. (The YAML key stays `mappings` - it reads better in the file.)
	Rules []Rule `yaml:"mappings"`

	timeout      time.Duration
	bindPassword string
}

// Rule is one directory-query → portal-group mapping.
type Rule struct {
	// Group is the portal group's display name.
	Group string `yaml:"group"`
	// GroupKey is the group's STABLE identity - what the portal's group row is keyed on - and
	// defaults to a slug of Group. Set it explicitly for either of two reasons:
	//
	//  1. To RELABEL a group without losing it. Change Group with GroupKey pinned and the existing
	//     group is renamed; without it, the slug changes and you get a second, empty group.
	//  2. To point SEVERAL rules at ONE group. This is the common Active Directory shape - one
	//     team, where a subset may write:
	//
	//         - group: Engineering
	//           group_key: engineering
	//           role: read
	//           filter: "(memberOf=CN=K8s-Eng,OU=Groups,DC=example,DC=lab)"
	//         - group: Engineering
	//           group_key: engineering
	//           role: write
	//           filter: "(memberOf=CN=K8s-Eng-Admins,OU=Groups,DC=example,DC=lab)"
	//
	//     Both rules land users in one group, so the whole team shares cluster access, and a user
	//     matching both gets write (highest role wins). Without this they would be two unrelated
	//     groups whose members cannot see each other's clusters at all.
	GroupKey string `yaml:"group_key"`
	// Role is what members matched by this rule get on each other's clusters: read | write.
	Role domain.GroupRole `yaml:"role"`
	// Filter is a raw LDAP filter evaluated against the authenticating user's own entry. This is
	// where the generality lives - anything the directory can express works:
	//
	//	(memberOf=CN=K8s-Eng,OU=Groups,DC=example,DC=lab)                 direct membership
	//	(memberOf:1.2.840.113556.1.4.1941:=CN=K8s-Eng,OU=Groups,DC=...)         AD nested groups
	//	(department=Engineering)                                                any attribute
	//	(&(department=Eng)(!(title=Contractor)))                                composed
	Filter string `yaml:"filter"`
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ldap: read config %s: %w", path, err)
	}
	return Parse(b)
}

// Parse validates a config document. Split from Load so tests need no temp files (and mirroring
// catalog.Parse / catalog.Default).
func Parse(b []byte) (*Config, error) {
	var c Config
	// KnownFields makes a typo'd key a boot error rather than a silently ignored mapping rule -
	// the kind of mistake that otherwise shows up as "why does nobody get the write role".
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("ldap: parse config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if len(c.URLs) == 0 {
		return fmt.Errorf("ldap: urls is required (e.g. [\"ldap://dc1.example.internal\"])")
	}
	tls := true
	if c.StartTLS != nil {
		tls = *c.StartTLS
	}
	for _, raw := range c.URLs {
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("ldap: urls: %q is not a valid URL: %w", raw, err)
		}
		switch u.Scheme {
		case "ldaps": // implicit TLS
		case "ldap":
			// A simple bind sends the password in the clear. Refusing by default turns a silent
			// credential leak - the service account's AND every user's, to anyone on the path -
			// into a boot error the operator has to consciously override.
			if !tls && !c.Insecure {
				return fmt.Errorf("ldap: urls: %q would bind over plaintext: set start_tls: true (recommended), use ldaps://, or acknowledge it with insecure: true", raw)
			}
		default:
			return fmt.Errorf("ldap: urls: %q has scheme %q (want ldap:// or ldaps://)", raw, u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("ldap: urls: %q has no host", raw)
		}
	}
	if c.UserBaseDN == "" {
		return fmt.Errorf("ldap: user_base_dn is required (e.g. \"DC=example,DC=lab\")")
	}
	if c.UserFilter == "" {
		c.UserFilter = defaultUserFilter
	}
	// Exactly one %s, and no other verb: the filter is fed to fmt.Sprintf with the escaped login,
	// so a stray %d or a second %s yields a corrupt filter (%!d(string=...)) that silently matches
	// nothing - i.e. "nobody can log in", with no clue why.
	if n := strings.Count(c.UserFilter, "%s"); n != 1 {
		return fmt.Errorf("ldap: user_filter must contain exactly one %%s placeholder for the login, found %d: %s", n, c.UserFilter)
	}
	if strings.Count(c.UserFilter, "%") != 1 {
		return fmt.Errorf("ldap: user_filter may contain no format verb other than the single %%s: %s", c.UserFilter)
	}
	if c.UsernameAttr == "" {
		c.UsernameAttr = defaultUsernameAttr
	}
	if c.DisplayNameAttr == "" {
		c.DisplayNameAttr = defaultDisplayAttr
	}
	if c.EmailAttr == "" {
		c.EmailAttr = defaultEmailAttr
	}
	if c.BindEnvVar == "" {
		c.BindEnvVar = defaultBindEnvVar
	}
	// Resolved here, but NOT required here: whether a bind password must be present is a property of
	// the CLIENT, not of the document. The fake directory parses the same file and never binds to
	// anything, so demanding one would break `KAAS_AUTH=ldap KAAS_LDAP=fake` - the path that exists
	// precisely so a config can be validated without credentials or a reachable DC. New() enforces
	// it (see ldap.go).
	c.bindPassword = os.Getenv(c.BindEnvVar)
	c.timeout = defaultTimeout
	if c.Timeout != "" {
		d, err := time.ParseDuration(c.Timeout)
		if err != nil {
			return fmt.Errorf("ldap: timeout %q: %w", c.Timeout, err)
		}
		if d <= 0 {
			return fmt.Errorf("ldap: timeout must be positive, got %s", c.Timeout)
		}
		c.timeout = d
	}
	if c.CAFile != "" {
		if _, err := os.Stat(c.CAFile); err != nil {
			return fmt.Errorf("ldap: ca_file %s: %w", c.CAFile, err)
		}
	}
	names := make(map[string]string, len(c.Rules))  // group_key -> display name
	roles := make(map[string]string, len(c.Rules))  // group_key + "/" + role -> where it was seen
	labels := make(map[string]string, len(c.Rules)) // display name -> group_key
	for i := range c.Rules {
		m := &c.Rules[i]
		where := fmt.Sprintf("ldap: mappings[%d]", i)
		if m.Group == "" {
			return fmt.Errorf("%s: group is required", where)
		}
		if m.GroupKey == "" {
			m.GroupKey = slugify(m.Group)
		}
		if m.GroupKey == "" {
			return fmt.Errorf("%s: group %q has no usable group_key - set one explicitly", where, m.Group)
		}
		if m.Filter == "" {
			return fmt.Errorf("%s (%s): filter is required", where, m.GroupKey)
		}
		if !m.Role.Valid() {
			return fmt.Errorf("%s (%s): role %q is not valid (want %q or %q - a directory rule cannot grant platform admin)",
				where, m.GroupKey, m.Role, domain.GroupRoleRead, domain.GroupRoleWrite)
		}
		// Rules sharing a group_key are one group, so they must agree on what it is called -
		// otherwise the group's name depends on which rule seeded it first.
		if prev, ok := names[m.GroupKey]; ok && prev != m.Group {
			return fmt.Errorf("%s: group_key %q is used with two different group names (%q and %q) - they are one group, so pick one name",
				where, m.GroupKey, prev, m.Group)
		}
		names[m.GroupKey] = m.Group
		// ...and conversely, one display name must not be two groups: groups.name is unique, so the
		// second would fail to seed at boot with a much less obvious error than this one.
		if prev, ok := labels[m.Group]; ok && prev != m.GroupKey {
			return fmt.Errorf("%s: group name %q is used by two different group_keys (%q and %q) - group names must be unique",
				where, m.Group, prev, m.GroupKey)
		}
		labels[m.Group] = m.GroupKey
		// Two rules granting the same role on the same group are redundant: the second can never
		// change the outcome. Say so rather than silently evaluating a filter for nothing.
		rk := m.GroupKey + "/" + string(m.Role)
		if prev, ok := roles[rk]; ok {
			return fmt.Errorf("%s: group %q already has a %q rule at %s - combine their filters instead, e.g. (|(filterA)(filterB))",
				where, m.GroupKey, m.Role, prev)
		}
		roles[rk] = where
	}
	// An empty mapping list is legal: everyone in user_base_dn may log in, nobody is grouped, and
	// they each own their own clusters. That is a coherent deployment, so it is not an error.
	return nil
}

// slugify derives a default group_key from a display name: lowercase, with every run of
// non-alphanumerics collapsed to a single '-'. "Platform Engineering" -> "platform-engineering".
//
// It only ever produces the DEFAULT. Once a group exists, its key is what identifies it, so an
// operator who wants to relabel a group pins group_key explicitly rather than letting the slug move
// underneath them.
func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// Mappings is the portal-facing view of the rules, for boot-time group seeding and the fake
// directory. The filters stay in here.
func (c *Config) Mappings() []authn.Mapping {
	out := make([]authn.Mapping, 0, len(c.Rules))
	for _, m := range c.Rules {
		out = append(out, authn.Mapping{GroupKey: m.GroupKey, Group: m.Group, Role: m.Role})
	}
	return out
}

// UseStartTLS reports whether an ldap:// connection should be upgraded before binding.
func (c *Config) UseStartTLS() bool { return c.StartTLS == nil || *c.StartTLS }

// Timeouts and the resolved bind password, for the client half (see ldap.go).
func (c *Config) timeoutOrDefault() time.Duration { return c.timeout }
