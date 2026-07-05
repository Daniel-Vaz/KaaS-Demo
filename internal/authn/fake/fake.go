// Package fake is the in-memory directory behind KAAS_LDAP=fake: it makes the whole Active
// Directory flow - JIT account provisioning, group seeding, membership sync, role assignment -
// demoable with `make up-fake` and testable with no domain controller anywhere.
//
// It synthesizes its users FROM THE DEPLOYMENT'S OWN MAPPING RULES rather than from a hardcoded
// list: one user per configured rule (matching exactly that rule), one matching every rule at
// once, and one matching none. So the fake directory always has a user that demonstrates each rule
// the operator actually wrote, and pointing KAAS_LDAP=fake at a real ldap.yaml is a dry run of
// that config's group/role wiring before you aim it at a live DC.
//
// Everything here is derived once at construction and never mutated, which is what makes it safe
// under `make up-scale API=3`: every replica synthesizes the identical directory from the identical
// config, so no login depends on which replica served it (see CLAUDE.md, horizontal scaling).
package fake

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/authn"
)

// Password is the one credential every synthetic user shares. It is a fixture, not a secret - the
// fake directory only ever exists in a deployment that has explicitly asked for KAAS_LDAP=fake.
const Password = "demo"

// Directory is a synthetic, immutable Active Directory.
type Directory struct {
	mappings []authn.Mapping
	users    map[string]*authn.Identity
}

// New builds the fake directory from the configured mapping rules.
//
// The synthetic roster, for a config whose rules grant <group_key>/<role>:
//
//	ad-<group_key>-<role>   one per rule; matches exactly that rule
//	ad-everyone             matches every rule at once - so if two rules point at one group with
//	                        different roles, this user is what demonstrates highest-role-wins
//	ad-nobody               matches no rule (a real user with no group - still owns their clusters)
//
// Every one of them authenticates with Password. Roles come from the rules themselves, so a
// read-role rule yields a read-role member - the fake asserts nothing of its own about policy.
//
// Names are per (group_key, role) rather than per group, because several rules may share a group;
// config validation rejects two rules with the same pair, so these can't collide.
func New(mappings []authn.Mapping) *Directory {
	d := &Directory{
		mappings: append([]authn.Mapping(nil), mappings...),
		users:    make(map[string]*authn.Identity, len(mappings)+2),
	}
	for _, m := range mappings {
		u := fmt.Sprintf("ad-%s-%s", strings.ToLower(m.GroupKey), strings.ToLower(string(m.Role)))
		d.users[u] = &authn.Identity{
			Username:    u,
			DisplayName: fmt.Sprintf("Fake %s (%s)", m.Group, m.Role),
			Email:       u + "@example.invalid",
			Groups:      []authn.GroupClaim{{GroupKey: m.GroupKey, Role: m.Role}},
		}
	}
	every := &authn.Identity{
		Username:    "ad-everyone",
		DisplayName: "Fake Everyone",
		Email:       "ad-everyone@example.invalid",
	}
	for _, m := range mappings {
		every.Groups = append(every.Groups, authn.GroupClaim{GroupKey: m.GroupKey, Role: m.Role})
	}
	d.users[every.Username] = every
	d.users["ad-nobody"] = &authn.Identity{
		Username:    "ad-nobody",
		DisplayName: "Fake Nobody",
		Email:       "ad-nobody@example.invalid",
	}
	return d
}

// Authenticate resolves a synthetic user. It mirrors the real implementation's contract exactly:
// every failure - unknown user, wrong password, empty password - is ErrInvalidCredentials and
// nothing else, so the fake can't pass a test the real one would fail.
func (d *Directory) Authenticate(_ context.Context, username, password string) (*authn.Identity, error) {
	// An empty password is rejected before anything else, exactly as the real implementation must:
	// a real LDAP simple bind with an empty password SUCCEEDS as an unauthenticated bind. Keeping
	// the check in both places means a test for it is meaningful against the fake.
	if password == "" {
		return nil, authn.ErrInvalidCredentials
	}
	u, ok := d.users[strings.ToLower(strings.TrimSpace(username))]
	if !ok || password != Password {
		return nil, authn.ErrInvalidCredentials
	}
	cp := *u
	cp.Groups = append([]authn.GroupClaim(nil), u.Groups...)
	return &cp, nil
}

// Mappings returns the configured rules, for boot-time group seeding.
func (d *Directory) Mappings() []authn.Mapping {
	return append([]authn.Mapping(nil), d.mappings...)
}

// Usernames lists the synthetic roster, sorted. The app logs it at boot so a `make up-fake` demo
// tells you who you can actually log in as.
func (d *Directory) Usernames() []string {
	out := make([]string, 0, len(d.users))
	for u := range d.users {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}
