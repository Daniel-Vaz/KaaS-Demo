// Package authn is the directory-authentication seam: verifying a username/password against an
// external identity source (Active Directory / LDAP) and reporting back who the user is and which
// portal groups they belong to.
//
// It is a REQUEST-DRIVEN seam, like internal/shell and internal/kube - the reconcile loop never
// touches it. It exists alongside, not instead of, the local accounts in internal/auth: a
// deployment picks a mechanism with KAAS_AUTH (local|ldap) and the fake/real axis with KAAS_LDAP
// (fake|real), the same two-axis split the infrastructure providers use (see CLAUDE.md).
//
// The seam deliberately says nothing about HOW a directory decides group membership. An
// Authenticator returns GroupClaims keyed by the mapping rule that produced them; resolving those
// keys to portal groups, provisioning the account and reconciling its memberships is the app
// layer's job (see internal/app.syncDirectoryUser). That keeps the LDAP-shaped concerns - filters,
// binds, DNs - from leaking into the tenancy model.
package authn

import (
	"context"
	"errors"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// ErrInvalidCredentials is every authentication failure, collapsed into one error on purpose: a
// wrong password, an unknown user and a disabled account must be indistinguishable to the caller,
// or /auth/login becomes a directory-enumeration oracle. The app layer maps it to its own
// ErrInvalidCredentials (a 401); it never reaches the client as text.
//
// This package deliberately has no sentinel for "user not found" - that distinction only exists
// inside an implementation, where it is used to skip the bind (see internal/authn/ldap).
var ErrInvalidCredentials = errors.New("authn: invalid credentials")

// Identity is what a directory resolves a credential into: who the user is, and what the
// deployment's mapping rules say about them. It carries no portal IDs - the app layer maps
// Username to a user row and each GroupClaim.Key to a group row.
type Identity struct {
	// Username is the normalized (lowercased) login name, taken from the configured username
	// attribute rather than whatever the user typed - so "DVaz", "dvaz" and a UPN that resolves to
	// the same directory entry all land on one portal account.
	Username    string
	DisplayName string
	Email       string
	// Groups is one claim per mapping rule the user matched. May be empty: a user who authenticates
	// but matches no rule is a valid, group-less account (they still own their own clusters).
	Groups []GroupClaim
}

// GroupClaim is one mapping rule the user matched: which group it puts them in, and with what role.
//
// GroupKey is the group's STABLE identity, not its display name - an operator may relabel a group
// in the config without meaning to point at a different one. Several rules may share a GroupKey,
// which is how "everyone in K8s-Eng can read, K8s-Eng-Admins can write" is expressed as one team
// rather than two disconnected ones; a user matching both is resolved highest-role-wins.
type GroupClaim struct {
	GroupKey string
	Role     domain.GroupRole
}

// Mapping is the portal-facing shape of one configured rule: enough for the app layer to
// pre-create the groups a directory can put people in, before anyone has logged in. The rule's
// filter - the part that decides who matches - stays inside the Authenticator.
//
// Mappings are per RULE, so two rules sharing a GroupKey appear twice here with the same Group and
// different Roles. Callers seeding groups must dedupe on GroupKey.
type Mapping struct {
	GroupKey string           // stable group identity; matches GroupClaim.GroupKey
	Group    string           // group display name
	Role     domain.GroupRole // role this rule grants; carried here so the fake directory can echo it
}

// Authenticator verifies a credential against a directory.
//
// Authenticate returns ErrInvalidCredentials for any authentication failure - a wrong password, an
// unknown user, a disabled account - so callers can't turn it into an account oracle. Other errors
// (an unreachable directory, a misconfigured bind account) are returned as-is: those are the
// platform's fault, not the user's, and must not be reported as a bad password.
//
// Implementations must be safe for concurrent use and must not hold any per-replica state that a
// request on another replica would need (see CLAUDE.md, horizontal scaling).
type Authenticator interface {
	Authenticate(ctx context.Context, username, password string) (*Identity, error)
	// Mappings returns every rule the deployment has configured, for boot-time group seeding.
	Mappings() []Mapping
}
