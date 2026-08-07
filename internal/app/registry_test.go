package app

import (
	"os"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/registry"
)

// TestRegistryAuthNotManagedWithoutKaasAuth is the guard against locking every user out of the
// registry. The WORKER - the only process that runs EnsurePlatform in real mode - is deliberately
// given neither KAAS_AUTH nor the mounted ldap.yaml, so reading the "local" default as authoritative
// there would rewrite a directory-authenticated registry's auth_mode back to its own user database.
func TestRegistryAuthNotManagedWithoutKaasAuth(t *testing.T) {
	t.Setenv("KAAS_REGISTRY_URL", "https://registry.example")
	os.Unsetenv("KAAS_AUTH")

	s, err := registryFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if s.ManageAuth {
		t.Error("a process with no KAAS_AUTH claims authority over the registry's auth configuration")
	}
}

// TestRegistryAuthManagedWhenToldLocal: a deployment that genuinely says "local" still manages its
// own auth - standing down must be about ABSENCE, not about the value.
func TestRegistryAuthManagedWhenToldLocal(t *testing.T) {
	t.Setenv("KAAS_REGISTRY_URL", "https://registry.example")
	t.Setenv("KAAS_AUTH", "local")

	s, err := registryFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !s.ManageAuth || s.AuthMode != registry.AuthLocal {
		t.Errorf("ManageAuth=%v AuthMode=%q, want true/local", s.ManageAuth, s.AuthMode)
	}
}

// TestRegistryAuthStandsDownWhenDirectoryConfigMissing: KAAS_AUTH=ldap with no readable ldap.yaml is
// the other way a process ends up with a non-authoritative view. It must not write the local
// fallback it computed to keep Validate happy.
func TestRegistryAuthStandsDownWhenDirectoryConfigMissing(t *testing.T) {
	t.Setenv("KAAS_REGISTRY_URL", "https://registry.example")
	t.Setenv("KAAS_AUTH", "ldap")
	t.Setenv("KAAS_LDAP_CONFIG", "/nonexistent/ldap.yaml")

	s, err := registryFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if s.ManageAuth {
		t.Error("ldap requested with no readable directory config still claims auth authority")
	}
}

// TestRegistryKeepsLdapModeWithoutDirectoryConfig pins the fix for a bug that was silent, permanent
// and destructive. The worker is given the auth MODE but not the directory's credentials, so it
// cannot write the registry's auth configuration - it stands down from that (ManageAuth). What it
// must NOT do is downgrade its own view to "local", which is what it used to do to satisfy Validate:
// AuthMode==local is what makes SyncAccess mint a LOCAL registry account per user, so the worker
// created a shadow account for every directory user - and because a registry refuses to change auth
// mode once its database holds users, that first sweep permanently locked the registry into the wrong
// mode. Nothing observable failed at the time.
func TestRegistryKeepsLdapModeWithoutDirectoryConfig(t *testing.T) {
	t.Setenv("KAAS_REGISTRY_URL", "http://harbor.lab:8090")
	// The worker's real shape: the REGISTRY-scoped mode, and no KAAS_AUTH at all. Plain KAAS_AUTH is
	// read by every seam - the Vault one refuses to start on ldap without directory settings - so
	// setting it on the worker stops it booting and nothing reconciles.
	t.Setenv("KAAS_REGISTRY_AUTH_MODE", registry.AuthLDAP)
	t.Setenv("KAAS_AUTH", "")
	t.Setenv("KAAS_LDAP_CONFIG", "")

	s, err := registryFromEnv()
	if err != nil {
		t.Fatalf("the worker's shape must be a valid configuration, got: %v", err)
	}
	if s.AuthMode != registry.AuthLDAP {
		t.Errorf("AuthMode = %q, want %q - a worker that calls itself local mints a shadow local account per directory user",
			s.AuthMode, registry.AuthLDAP)
	}
	if s.ManageAuth {
		t.Error("ManageAuth = true without directory settings; it would flip the registry to db_auth and lock everyone out")
	}
}

// TestWorkerStartsWithRegistryLdapModeAndNoDirectory is the regression guard for a worker that would
// not boot. The registry needs the auth mode on the worker, but KAAS_AUTH is read by EVERY seam, and
// the Vault one hard-fails on `ldap` without directory settings - so wiring the mode in as KAAS_AUTH
// took the whole worker down at start-up. In real mode the worker owns the reconcile loop, so the
// visible symptom was not "the registry is wrong": it was every new cluster sitting in Pending with
// an empty Activity tab, because nothing was reconciling at all.
func TestWorkerStartsWithRegistryLdapModeAndNoDirectory(t *testing.T) {
	t.Setenv("KAAS_REGISTRY_URL", "http://harbor.lab:8090")
	t.Setenv("KAAS_REGISTRY_AUTH_MODE", registry.AuthLDAP)
	t.Setenv("KAAS_AUTH", "")
	t.Setenv("KAAS_LDAP_CONFIG", "")

	// The registry takes the directory mode...
	rs, err := registryFromEnv()
	if err != nil {
		t.Fatalf("registry settings: %v", err)
	}
	if rs.AuthMode != registry.AuthLDAP {
		t.Errorf("registry AuthMode = %q, want %q", rs.AuthMode, registry.AuthLDAP)
	}
	// ...and every OTHER seam keyed on KAAS_AUTH is untouched, so the worker still starts. Vault is
	// the one that fails loudly, so it stands in for the rest.
	if _, err := vaultFromEnv(); err != nil {
		t.Fatalf("vault settings must stay valid on the worker, got: %v", err)
	}
}
