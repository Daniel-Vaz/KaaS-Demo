package ldap

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// discard is a logger for tests that assert on behaviour, not output.
func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// minimal is a config that must always parse, so each test below can vary exactly one thing.
const minimal = `
urls: ["ldaps://dc1.example.lab"]
user_base_dn: "DC=example,DC=lab"
`

func TestParseDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.UserFilter != defaultUserFilter {
		t.Errorf("UserFilter = %q, want the AD default %q", c.UserFilter, defaultUserFilter)
	}
	if c.UsernameAttr != defaultUsernameAttr {
		t.Errorf("UsernameAttr = %q, want %q", c.UsernameAttr, defaultUsernameAttr)
	}
	if c.timeoutOrDefault() != defaultTimeout {
		t.Errorf("timeout = %s, want %s", c.timeoutOrDefault(), defaultTimeout)
	}
	if !c.UseStartTLS() {
		t.Error("UseStartTLS = false; StartTLS must default to on")
	}
	if len(c.Mappings()) != 0 {
		t.Errorf("Mappings = %v, want none", c.Mappings())
	}
}

// A config with no rules is legal: everyone in the base DN may log in, nobody is grouped. It must
// not be mistaken for a misconfiguration.
func TestParseNoMappingsIsValid(t *testing.T) {
	if _, err := Parse([]byte(minimal)); err != nil {
		t.Fatalf("a mapping-less config must parse: %v", err)
	}
}

func TestParseMappings(t *testing.T) {
	c, err := Parse([]byte(minimal + `
mappings:
  - group: "Platform"
    group_key: platform
    role: write
    filter: "(memberOf=CN=K8s-Admins,OU=Groups,DC=example,DC=lab)"
  - group: "Engineering"
    group_key: eng
    role: read
    filter: "(department=Engineering)"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := c.Mappings()
	if len(got) != 2 {
		t.Fatalf("Mappings = %d, want 2", len(got))
	}
	if got[0].GroupKey != "platform" || got[0].Group != "Platform" || got[0].Role != domain.GroupRoleWrite {
		t.Errorf("Mappings[0] = %+v", got[0])
	}
	if got[1].Role != domain.GroupRoleRead {
		t.Errorf("Mappings[1].Role = %q, want read", got[1].Role)
	}
}

// group_key defaults to a slug of the display name, so the common config needs no key at all.
func TestParseGroupKeyDefaultsToSlug(t *testing.T) {
	c, err := Parse([]byte(minimal + `
mappings:
  - group: "Platform Engineering"
    role: write
    filter: "(a=b)"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := c.Mappings()[0].GroupKey; got != "platform-engineering" {
		t.Errorf("default group_key = %q, want platform-engineering", got)
	}
}

// The topology this exists for: one team, two AD groups, different roles, ONE portal group.
func TestParseSharedGroupKey(t *testing.T) {
	c, err := Parse([]byte(minimal + `
mappings:
  - group: "Engineering"
    group_key: eng
    role: read
    filter: "(memberOf=CN=K8s-Eng,DC=example,DC=lab)"
  - group: "Engineering"
    group_key: eng
    role: write
    filter: "(memberOf=CN=K8s-Eng-Admins,DC=example,DC=lab)"
`))
	if err != nil {
		t.Fatalf("two rules sharing a group_key must parse: %v", err)
	}
	got := c.Mappings()
	if len(got) != 2 {
		t.Fatalf("Mappings = %d, want 2 (one per rule)", len(got))
	}
	if got[0].GroupKey != got[1].GroupKey {
		t.Error("both rules must carry the same group_key")
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Platform":                "platform",
		"Platform Engineering":    "platform-engineering",
		"K8s Admins (EMEA)":       "k8s-admins-emea",
		"  leading and trailing ": "leading-and-trailing",
		"Multiple   Spaces":       "multiple-spaces",
		"already-slugged":         "already-slugged",
	} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseTimeout(t *testing.T) {
	c, err := Parse([]byte(minimal + "timeout: 3s\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.timeoutOrDefault() != 3*time.Second {
		t.Errorf("timeout = %s, want 3s", c.timeoutOrDefault())
	}
}

// Plaintext ldap:// sends the bind password in the clear. It must take a deliberate opt-in, not a
// silent default - this is the check standing between a demo config and a leaked service account.
func TestParsePlaintextRequiresOptIn(t *testing.T) {
	plain := `
urls: ["ldap://dc1.example.lab"]
user_base_dn: "DC=example,DC=lab"
start_tls: false
`
	if _, err := Parse([]byte(plain)); err == nil {
		t.Fatal("plaintext ldap:// without start_tls or insecure must be rejected")
	}
	if _, err := Parse([]byte(plain + "insecure: true\n")); err != nil {
		t.Errorf("insecure: true is the documented opt-in and must parse: %v", err)
	}
	// StartTLS on (the default) is fine over ldap://: the bind happens after the upgrade.
	if _, err := Parse([]byte("urls: [\"ldap://dc1\"]\nuser_base_dn: \"DC=x\"\n")); err != nil {
		t.Errorf("ldap:// with default start_tls must parse: %v", err)
	}
	// ldaps:// is already TLS, so start_tls: false is irrelevant, not an error.
	if _, err := Parse([]byte("urls: [\"ldaps://dc1\"]\nuser_base_dn: \"DC=x\"\nstart_tls: false\n")); err != nil {
		t.Errorf("ldaps:// with start_tls: false must parse: %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"no urls", "user_base_dn: \"DC=x\"\n", "urls is required"},
		{"bad scheme", "urls: [\"http://dc1\"]\nuser_base_dn: \"DC=x\"\n", "scheme"},
		{"no host", "urls: [\"ldaps://\"]\nuser_base_dn: \"DC=x\"\n", "no host"},
		{"no base dn", "urls: [\"ldaps://dc1\"]\n", "user_base_dn is required"},
		{"unknown field", minimal + "bind_password: hunter2\n", "field bind_password not found"},

		// The %s rules matter: the filter is fmt.Sprintf'd with the login, so a missing, doubled or
		// foreign verb yields a filter that silently matches nobody.
		{"filter no placeholder", minimal + "user_filter: \"(objectClass=user)\"\n", "exactly one %s"},
		{"filter two placeholders", minimal + "user_filter: \"(|(uid=%s)(cn=%s))\"\n", "exactly one %s"},
		{"filter foreign verb", minimal + "user_filter: \"(&(uid=%s)(id=%d))\"\n", "no format verb other than"},

		{"mapping no group", minimal + "mappings:\n  - group_key: k\n    role: read\n    filter: \"(a=b)\"\n", "group is required"},
		{"mapping no filter", minimal + "mappings:\n  - group: G\n    role: read\n", "filter is required"},
		{"mapping bad role", minimal + "mappings:\n  - group: G\n    role: admin\n    filter: \"(a=b)\"\n", "role \"admin\" is not valid"},

		// One group_key is one group, so its name can't depend on which rule seeded it first.
		{"group_key with two names", minimal + "mappings:\n  - group: A\n    group_key: k\n    role: read\n    filter: \"(a=b)\"\n  - group: B\n    group_key: k\n    role: write\n    filter: \"(c=d)\"\n", "two different group names"},
		// ...and one name can't be two groups: groups.name is unique, so the second would fail to
		// seed at boot with a far less obvious error.
		{"name with two group_keys", minimal + "mappings:\n  - group: G\n    group_key: j\n    role: read\n    filter: \"(a=b)\"\n  - group: G\n    group_key: k\n    role: write\n    filter: \"(c=d)\"\n", "two different group_keys"},
		// Two rules granting the same role on the same group: the second can never change anything.
		{"duplicate group+role", minimal + "mappings:\n  - group: G\n    role: read\n    filter: \"(a=b)\"\n  - group: G\n    role: read\n    filter: \"(c=d)\"\n", "combine their filters"},
		{"bad timeout", minimal + "timeout: soon\n", "timeout"},
		{"zero timeout", minimal + "timeout: 0s\n", "must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("Parse must reject %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// role: admin is deliberately NOT a thing - platform admin stays a local, seed-only flag, because
// quota.Allocated skips admins and a directory-driven admin toggle would corrupt the conserved-pool
// invariant. Guard it here so nobody adds it back without meeting that problem.
func TestParseRejectsAdminRole(t *testing.T) {
	_, err := Parse([]byte(minimal + "mappings:\n  - group: G\n    role: admin\n    filter: \"(a=b)\"\n"))
	if err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("role: admin must be rejected, got %v", err)
	}
}

// A bind_dn without its password is the single most likely misconfiguration, and left to itself it
// fails at the DC with a generic error the first time someone tries to sign in. New catches it at
// boot and names the variable.
//
// It is New's job and not Parse's: the DOCUMENT is valid either way. The fake directory reads the
// same file and never binds to anything, so requiring a password to parse would break
// `KAAS_AUTH=ldap KAAS_LDAP=fake` - the path whose whole purpose is validating a config without
// credentials or a reachable DC.
func TestNewRequiresBindPassword(t *testing.T) {
	y := minimal + "bind_dn: \"CN=svc,DC=example,DC=lab\"\n"

	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("a bind_dn with no password must still PARSE (the fake directory needs this): %v", err)
	}
	_, err = New(c, discard())
	if err == nil || !strings.Contains(err.Error(), defaultPasswordEnv) {
		t.Fatalf("New = %v, want it rejected and naming %s", err, defaultPasswordEnv)
	}

	t.Setenv(defaultPasswordEnv, "s3cret")
	c, err = Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse with the password set: %v", err)
	}
	if c.bindPassword != "s3cret" {
		t.Errorf("bindPassword = %q, want it resolved from the env", c.bindPassword)
	}
	if _, err := New(c, discard()); err != nil {
		t.Errorf("New with the password set: %v", err)
	}
}

// An anonymous-bind config (no bind_dn) needs no password at all.
func TestNewAnonymousBindNeedsNoPassword(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := New(c, discard()); err != nil {
		t.Errorf("New without a bind_dn: %v", err)
	}
}

func TestParseCustomPasswordEnv(t *testing.T) {
	t.Setenv("MY_LDAP_PW", "abc")
	c, err := Parse([]byte(minimal + "bind_dn: \"CN=svc,DC=x\"\nbind_password_env: MY_LDAP_PW\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.bindPassword != "abc" {
		t.Errorf("bindPassword = %q, want it read from MY_LDAP_PW", c.bindPassword)
	}
}

// The bind password must never be readable from the config document itself.
func TestPasswordIsNotAConfigField(t *testing.T) {
	_, err := Parse([]byte(minimal + "bind_password: hunter2\n"))
	if err == nil {
		t.Fatal("bind_password must not be an accepted field - the password belongs in the env")
	}
}
