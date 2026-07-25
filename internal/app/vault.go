package app

// HashiCorp Vault wiring - the deployment-level selection of the vault.Manager seam, mirroring
// internal/app/dns.go. See internal/vault for the model: one central Vault, per-cluster KV paths and
// policies that mirror the portal's access, converged by the reconcile loop and a leader-elected sweep.
//
// Credential placement follows the same split as DNS and the LDAP bind password: the WORKER holds the
// broad management token (it provisions mounts, policies, identity and per-cluster paths and already
// holds every tenant's secrets), while the API holds only a narrow minter token (MintUserToken, for
// the "View in Vault" handoff). Both build a Manager from the same env - each calls only the subset
// its token permits. The Fake is used whenever KAAS_VAULT is unset, so `make up-fake` demos the whole
// flow with no Vault.

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	authnldap "github.com/Daniel-Vaz/KaaS-demo/internal/authn/ldap"
	"github.com/Daniel-Vaz/KaaS-demo/internal/events"
	"github.com/Daniel-Vaz/KaaS-demo/internal/vault"
	"github.com/Daniel-Vaz/KaaS-demo/internal/vault/hcvault"
)

// vaultFromEnv reads the deployment's Vault settings. The auth backend Vault configures follows the
// portal's own KAAS_AUTH: local → userpass, ldap → the same directory the portal authenticates
// against (translated from the mounted ldap.yaml). Unset KAAS_VAULT_ADDR leaves the Fake in place.
func vaultFromEnv() (vault.Settings, error) {
	s := vault.Settings{
		Addr:     os.Getenv("KAAS_VAULT_ADDR"),
		Mount:    getenv("KAAS_VAULT_MOUNT", vault.DefaultMount),
		Token:    os.Getenv("KAAS_VAULT_TOKEN"),
		UIURL:    getenv("KAAS_VAULT_UI_URL", os.Getenv("KAAS_VAULT_ADDR")),
		AuthMode: strings.ToLower(getenv("KAAS_AUTH", AuthLocal)),
		TokenTTL: envDuration("KAAS_VAULT_TOKEN_TTL", 0),
	}
	if s.AuthMode == vault.AuthLDAP {
		ldapCfg, err := vaultLDAPAuth()
		if err != nil {
			return vault.Settings{}, fmt.Errorf("KAAS_VAULT: %w", err)
		}
		s.LDAP = ldapCfg
	}
	return s.Validate()
}

// vaultLDAPAuth translates the portal's directory config (the same ldap.yaml the API mounts) into the
// subset Vault's ldap auth method needs, so Vault authenticates users against the SAME directory with
// the SAME login attribute the portal uses. Returns nil settings without error when the file is
// absent (a worker that manages Vault but was not given the directory config falls back to userpass).
func vaultLDAPAuth() (*vault.LDAPAuth, error) {
	path := getenv("KAAS_LDAP_CONFIG", "/etc/kaas/ldap.yaml")
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	cfg, err := authnldap.Load(path)
	if err != nil {
		return nil, err
	}
	url := ""
	if len(cfg.URLs) > 0 {
		url = cfg.URLs[0]
	}
	startTLS := true
	if cfg.StartTLS != nil {
		startTLS = *cfg.StartTLS
	}
	return &vault.LDAPAuth{
		URL:          url,
		StartTLS:     startTLS,
		BindDN:       cfg.BindDN,
		BindPassword: os.Getenv(cfg.BindEnvVar),
		UserDN:       cfg.UserBaseDN,
		UserAttr:     getenv2(cfg.UsernameAttr, "sAMAccountName"),
		InsecureTLS:  cfg.InsecureSkipVerify,
	}, nil
}

// buildVaultManager selects the Vault seam from KAAS_VAULT (fake|real). Only a process given a token
// builds a real one; the API and worker both go through here, each using the subset its token allows.
func buildVaultManager(log *slog.Logger, sink events.Sink, s vault.Settings) (vault.Manager, error) {
	switch strings.ToLower(getenv("KAAS_VAULT", "fake")) {
	case "fake", "":
		return vault.NewFake(log), nil
	case "real":
		if !s.Enabled() {
			return nil, fmt.Errorf("KAAS_VAULT=real needs KAAS_VAULT_ADDR")
		}
		return hcvault.New(hcvault.Config{
			Settings: s,
			Insecure: envBool("KAAS_VAULT_INSECURE", false),
			Events:   sink,
			Log:      log,
		})
	default:
		return nil, fmt.Errorf("unknown KAAS_VAULT %q (want fake|real)", os.Getenv("KAAS_VAULT"))
	}
}

// getenv2 returns v when non-empty, else def - a local helper for defaulting a struct field that is
// already a value (getenv reads the environment; this defaults an in-hand string).
func getenv2(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
