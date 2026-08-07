// Package hcvault is the real vault.Manager: a thin net/http client against HashiCorp Vault's REST
// API (the same shape as internal/netbox). It is built only where a Vault token lives - the worker
// (a broad management token driving EnsurePlatform/EnsureCluster/ReleaseCluster/SyncAccess) and the
// API (a narrow minter token calling only MintUserToken). Every operation is an idempotent upsert, so
// it is safe to re-run on every reconcile tick and every SyncAccess sweep.
//
// Shortcuts, in the repo's style (documented rather than hidden):
//   - The External Secrets auth uses a per-cluster JWT auth mount seeded with the cluster's public
//     signing keys, so Vault validates the ESO token offline. Production would use Vault Kubernetes
//     auth with a token reviewer (or a JWKS URL) instead of platform-copied keys.
//   - MintUserToken creates a child token with explicit policies; the minter token is expected to be
//     a token-role-restricted credential (allowed_policies = kaas-*). Production would front the whole
//     handoff with OIDC SSO so the API never handles Vault tokens.
package hcvault

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/events"
	"github.com/Daniel-Vaz/KaaS-demo/internal/vault"
)

// Config builds a Client.
type Config struct {
	Settings vault.Settings
	Insecure bool // skip TLS verification (a self-signed Vault in the demo)
	Events   events.Sink
	Log      *slog.Logger
}

// Client is the real vault.Manager.
type Client struct {
	addr   string
	mount  string
	token  string
	set    vault.Settings
	http   *http.Client
	events events.Sink
	log    *slog.Logger
}

// New builds a Client from validated settings.
func New(cfg Config) (*Client, error) {
	s, err := cfg.Settings.Validate()
	if err != nil {
		return nil, err
	}
	if !s.Enabled() {
		return nil, fmt.Errorf("hcvault: KAAS_VAULT_ADDR is required")
	}
	tr := &http.Transport{}
	if cfg.Insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &Client{
		addr:   strings.TrimRight(s.Addr, "/"),
		mount:  s.Mount,
		token:  s.Token,
		set:    s,
		http:   &http.Client{Timeout: 30 * time.Second, Transport: tr},
		events: cfg.Events,
		log:    cfg.Log,
	}, nil
}

// do performs one Vault API call. okCodes are the statuses treated as success (besides 2xx); a
// Vault "already exists" is commonly 400/409/204, so callers pass the codes that mean "already done".
func (c *Client) do(ctx context.Context, method, path string, body any, out any, okCodes ...int) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.addr+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("hcvault: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out != nil && len(data) > 0 {
			return json.Unmarshal(data, out)
		}
		return nil
	}
	for _, ok := range okCodes {
		if resp.StatusCode == ok {
			return nil
		}
	}
	return fmt.Errorf("hcvault: %s %s: %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
}

// emit puts a message on a CLUSTER's activity timeline, or logs it when the message is
// platform-scoped (clusterID ""). The split is forced by the schema: events.cluster_id is NOT NULL
// and foreign-keyed to clusters ON DELETE CASCADE, so the events table is per-cluster by
// construction and there is no platform-wide timeline to write to - the portal only ever renders an
// Activity tab for one cluster. An emit with no cluster ID is therefore not a harmless no-op but a
// guaranteed FK violation: the broker drops the message and logs a persist error that reads like a
// real database fault. Logging is the honest home for those, and it keeps the authAccessor warning
// below visible - the one message here whose whole purpose is to surface a silent misconfiguration.
func (c *Client) emit(clusterID, level, msg string) {
	if clusterID == "" {
		if c.log != nil {
			if level == "warn" {
				c.log.Warn(msg)
			} else {
				c.log.Info(msg)
			}
		}
		return
	}
	if c.events != nil {
		c.events.Emit(events.Event{ClusterID: clusterID, Source: "vault", Level: level, Message: msg})
	}
}

// EnsurePlatform enables the KV v2 mount, configures the auth backend matching the portal, writes the
// admin policy and creates the kaas-admins group. Idempotent: an already-enabled mount/auth returns a
// 400 "path is already in use" which is treated as success.
func (c *Client) EnsurePlatform(ctx context.Context) error {
	// KV v2 mount.
	if err := c.do(ctx, http.MethodPost, "/v1/sys/mounts/"+c.mount,
		map[string]any{"type": "kv", "options": map[string]string{"version": "2"}},
		nil, http.StatusBadRequest, http.StatusNoContent); err != nil {
		return err
	}
	// Admin policy + admins group (created empty; SyncAccess fills its members).
	if err := c.putPolicy(ctx, vault.PolicyAdmin, vault.AdminPolicyHCL(c.mount)); err != nil {
		return err
	}
	if err := c.putGroup(ctx, vault.GroupAdmins, []string{vault.PolicyAdmin}, nil); err != nil {
		return err
	}
	// Auth backend, mirroring KAAS_AUTH.
	switch c.set.AuthMode {
	case vault.AuthLDAP:
		if err := c.ensureAuth(ctx, "ldap", "ldap"); err != nil {
			return err
		}
		l := c.set.LDAP
		if l != nil {
			cfg := map[string]any{
				"url":          l.URL,
				"binddn":       l.BindDN,
				"bindpass":     l.BindPassword,
				"userdn":       l.UserDN,
				"userattr":     l.UserAttr,
				"starttls":     l.StartTLS,
				"insecure_tls": l.InsecureTLS,
			}
			if err := c.do(ctx, http.MethodPost, "/v1/auth/ldap/config", cfg, nil); err != nil {
				return err
			}
		}
	default: // AuthLocal → userpass (populated out-of-band; the portal's handoff mints tokens directly)
		if err := c.ensureAuth(ctx, "userpass", "userpass"); err != nil {
			return err
		}
	}
	c.emit("", "info", "vault: provisioned the platform Vault mount, auth backend and admin policy")
	return nil
}

// EnsureCluster writes a cluster's read/write policies, seeds a marker so the KV subtree is visible,
// and configures the per-cluster External-Secrets JWT auth mount + role bound to the read policy.
func (c *Client) EnsureCluster(ctx context.Context, cl *domain.Cluster, eso vault.ESOAuth) error {
	if err := c.putPolicy(ctx, vault.PolicyRead(cl.ID), vault.ReadPolicyHCL(c.mount, cl.ID)); err != nil {
		return err
	}
	if err := c.putPolicy(ctx, vault.PolicyWrite(cl.ID), vault.WritePolicyHCL(c.mount, cl.ID)); err != nil {
		return err
	}
	// A marker secret so the subtree exists and lists - real tenant secrets live alongside it.
	marker := fmt.Sprintf("/v1/%s/data/%s/.platform", c.mount, vault.ClusterPrefix(cl.ID))
	if err := c.do(ctx, http.MethodPost, marker,
		map[string]any{"data": map[string]any{"cluster": cl.Name, "managed_by": "kaas"}}, nil); err != nil {
		return err
	}
	// External Secrets auth: only wire it when the cluster actually reported its signing keys.
	if len(eso.PublicKeys) > 0 && eso.ServiceAcct != "" {
		if err := c.ensureESOAuth(ctx, cl.ID, eso); err != nil {
			return err
		}
	}
	c.emit(cl.ID, "info", fmt.Sprintf("provisioned Vault path and policies for cluster %q", cl.Name))
	return nil
}

// ensureESOAuth configures a per-cluster JWT auth mount seeded with the cluster's public signing keys
// and a role binding the ESO ServiceAccount to the cluster's READ policy - nothing else.
func (c *Client) ensureESOAuth(ctx context.Context, clusterID string, eso vault.ESOAuth) error {
	mount := "jwt-" + clusterID
	if err := c.ensureAuth(ctx, mount, "jwt"); err != nil {
		return err
	}
	if err := c.do(ctx, http.MethodPost, "/v1/auth/"+mount+"/config", map[string]any{
		"jwt_validation_pubkeys": eso.PublicKeys,
		"bound_issuer":           eso.Issuer,
	}, nil); err != nil {
		return err
	}
	// A projected ServiceAccount token's `sub` claim is colon-separated -
	// "system:serviceaccount:<namespace>:<name>" - so the "<namespace>/<name>" form must have its
	// slash converted, or Vault rejects every token with "invalid subject (sub) claim".
	return c.do(ctx, http.MethodPost, "/v1/auth/"+mount+"/role/"+vault.AuthRole(clusterID), map[string]any{
		"role_type":       "jwt",
		"user_claim":      "sub",
		"bound_subject":   "system:serviceaccount:" + strings.Replace(eso.ServiceAcct, "/", ":", 1),
		"bound_audiences": []string{"vault"},
		"token_policies":  []string{vault.PolicyRead(clusterID)},
		"token_ttl":       "1h",
	}, nil)
}

// ReleaseCluster removes a cluster's policies, ESO auth mount and KV subtree. Absence is success
// (404/400 treated as ok), so it is safe on every deleting tick.
func (c *Client) ReleaseCluster(ctx context.Context, cl *domain.Cluster) error {
	_ = c.do(ctx, http.MethodDelete, "/v1/sys/auth/jwt-"+cl.ID, nil, nil, http.StatusBadRequest, http.StatusNotFound)
	// Recursively delete the KV metadata (which drops all versions).
	if err := c.deleteKVTree(ctx, vault.ClusterPrefix(cl.ID)); err != nil {
		return err
	}
	for _, p := range []string{vault.PolicyRead(cl.ID), vault.PolicyWrite(cl.ID)} {
		if err := c.do(ctx, http.MethodDelete, "/v1/sys/policies/acl/"+p, nil, nil, http.StatusNotFound); err != nil {
			return err
		}
	}
	// The cluster row still exists here: releaseVault runs in PhaseDeleting, BEFORE DestroyCluster
	// and before the row is dropped - so the FK holds, and ON DELETE CASCADE takes the event with it.
	c.emit(cl.ID, "info", fmt.Sprintf("released Vault path and policies for cluster %q", cl.Name))
	return nil
}

// deleteKVTree recursively deletes a KV v2 subtree by listing metadata and deleting each leaf.
func (c *Client) deleteKVTree(ctx context.Context, prefix string) error {
	var lr struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	err := c.do(ctx, "LIST", "/v1/"+c.mount+"/metadata/"+prefix, nil, &lr, http.StatusNotFound)
	if err != nil {
		return err
	}
	for _, k := range lr.Data.Keys {
		child := strings.TrimRight(prefix, "/") + "/" + k
		if strings.HasSuffix(k, "/") {
			if err := c.deleteKVTree(ctx, strings.TrimRight(child, "/")); err != nil {
				return err
			}
			continue
		}
		if err := c.do(ctx, http.MethodDelete, "/v1/"+c.mount+"/metadata/"+child, nil, nil, http.StatusNotFound); err != nil {
			return err
		}
	}
	// Drop the folder marker itself.
	return c.do(ctx, http.MethodDelete, "/v1/"+c.mount+"/metadata/"+prefix, nil, nil, http.StatusNotFound)
}

// SyncAccess converges Vault's entities and identity groups to DesiredAccess(snap). It creates every
// entity (resolving its id), attaches its owned-cluster policies + the auth-mount alias, then writes
// each identity group with its policies and member entity ids.
func (c *Client) SyncAccess(ctx context.Context, snap vault.AccessSnapshot) error {
	desired := vault.DesiredAccess(snap)
	accessor, err := c.authAccessor(ctx)
	if err != nil {
		return err
	}
	ids := make(map[string]string, len(desired.Entities)) // entity name -> id
	for _, e := range desired.Entities {
		id, err := c.upsertEntity(ctx, e.Name, e.Policies)
		if err != nil {
			return err
		}
		ids[e.Name] = id
		if e.Alias != "" && accessor != "" {
			if err := c.ensureAlias(ctx, id, e.Alias, accessor); err != nil {
				return err
			}
		}
	}
	for _, g := range desired.Groups {
		memberIDs := make([]string, 0, len(g.Members))
		for _, m := range g.Members {
			if id := ids[m]; id != "" {
				memberIDs = append(memberIDs, id)
			}
		}
		if err := c.putGroup(ctx, g.Name, g.Policies, memberIDs); err != nil {
			return err
		}
	}
	return nil
}

// MintUserToken creates a short-lived child token carrying policies (the UI handoff token).
//
// It refuses to mint a token for a policy Vault does not have. Vault ACCEPTS auth/token/create with an
// unknown policy name - the token is issued, token/lookup-self even lists the name, and it grants
// nothing - so the platform would report a successful handoff and the user would land in the Vault UI
// on "preflight capability check returned 403 … path "<mount>/"", with nothing tying that back here.
// The policies are written by EnsurePlatform (kaas-admin) and EnsureCluster (the per-cluster pair), so
// their absence means the worker's provisioning never reached this Vault or its state was lost - a
// deployment fault worth surfacing at the moment it is detectable rather than one UI away.
func (c *Client) MintUserToken(ctx context.Context, policies []string, meta map[string]string) (string, error) {
	if err := c.assertPoliciesExist(ctx, policies); err != nil {
		return "", err
	}
	var out struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	body := map[string]any{
		"policies":  policies,
		"ttl":       c.set.TokenTTL.String(),
		"no_parent": true,
		"meta":      meta,
		"num_uses":  0,
	}
	if err := c.do(ctx, http.MethodPost, "/v1/auth/token/create", body, &out); err != nil {
		return "", err
	}
	return out.Auth.ClientToken, nil
}

// assertPoliciesExist fails when Vault definitively does not have one of the policies.
//
// The distinction that matters is 404 vs 403, and only 404 is evidence: reading an ACL policy needs
// `read` on sys/policies/acl/<name>, which the broad management token has and a narrow production
// minter (a token role restricted to allowed_policies = kaas-*) legitimately does not. A minter that
// cannot look gets 403 for present and absent policies alike, so treating that as absence would break
// exactly the deployment this package documents as the production shape. Not being allowed to check is
// not a reason to refuse - only a definite 404 is.
func (c *Client) assertPoliciesExist(ctx context.Context, policies []string) error {
	for _, p := range policies {
		code, err := c.probe(ctx, "/v1/sys/policies/acl/"+url.PathEscape(p))
		if err != nil {
			return err // a transport failure is a real failure; the caller retries
		}
		if code == http.StatusNotFound {
			return fmt.Errorf("hcvault: Vault at %s has no policy %q, so a token carrying it would grant "+
				"nothing - the worker's Vault provisioning has not reached this Vault (check that the "+
				"worker runs KAAS_VAULT=real against the same address, and that Vault has not lost its "+
				"state; a cluster's policies are rewritten by clearing its vault_wired marker)", c.addr, p)
		}
	}
	return nil
}

// probe issues a GET and reports the status code, for the cases where the code IS the answer and a
// non-2xx is not on its own an error. A transport failure is still an error.
func (c *Client) probe(ctx context.Context, path string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.addr+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Vault-Token", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("hcvault: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, nil
}

// --- small helpers ------------------------------------------------------------------------------

func (c *Client) putPolicy(ctx context.Context, name, hcl string) error {
	return c.do(ctx, http.MethodPut, "/v1/sys/policies/acl/"+name, map[string]string{"policy": hcl}, nil)
}

// ensureAuth enables an auth method at path with the given type, tolerating "already in use".
func (c *Client) ensureAuth(ctx context.Context, path, typ string) error {
	return c.do(ctx, http.MethodPost, "/v1/sys/auth/"+path, map[string]any{"type": typ},
		nil, http.StatusBadRequest, http.StatusNoContent)
}

// authAccessor returns the mount accessor of the configured auth backend, needed to alias entities.
//
// Decode the "data" sub-object, NOT the top level. GET /v1/sys/auth repeats the mount map at the top
// level for backwards compatibility, but merged in with the standard response envelope - request_id,
// lease_id and mount_type are strings, renewable a bool, lease_duration a number. Unmarshalling that
// into map[string]struct{Accessor string} fails on the first scalar ("json: cannot unmarshal string
// into Go value of type struct { Accessor string }"), so authAccessor returned an error for every
// call and SyncAccess converged NOTHING - no entity, no policy, no group - on every sweep. The bug
// was invisible in fake mode, which never exercises this path.
func (c *Client) authAccessor(ctx context.Context) (string, error) {
	var out struct {
		Data map[string]struct {
			Accessor string `json:"accessor"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/sys/auth", nil, &out); err != nil {
		return "", err
	}
	path := "userpass/"
	if c.set.AuthMode == vault.AuthLDAP {
		path = "ldap/"
	}
	m, ok := out.Data[path]
	if !ok || m.Accessor == "" {
		// Not fatal: SyncAccess still writes entities, policies and groups, and only skips the
		// login-time alias. But without the alias a user's login maps to no entity, so their
		// policies never apply - warn rather than fail silently, which is how this stayed hidden.
		c.emit("", "warn", fmt.Sprintf("vault: auth mount %q has no accessor - entity aliases skipped, logins will carry no cluster policies", strings.TrimSuffix(path, "/")))
		return "", nil
	}
	return m.Accessor, nil
}

// upsertEntity creates or updates an identity entity by name and returns its id.
func (c *Client) upsertEntity(ctx context.Context, name string, policies []string) (string, error) {
	body := map[string]any{"name": name}
	if policies != nil {
		body["policies"] = policies
	} else {
		body["policies"] = []string{}
	}
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	// POST /identity/entity upserts by name and returns the id on create; on update it returns 204,
	// so read the id back by name afterwards.
	if err := c.do(ctx, http.MethodPost, "/v1/identity/entity", body, &out); err != nil {
		return "", err
	}
	if out.Data.ID != "" {
		return out.Data.ID, nil
	}
	return c.entityID(ctx, name)
}

func (c *Client) entityID(ctx context.Context, name string) (string, error) {
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/identity/entity/name/"+name, nil, &out); err != nil {
		return "", err
	}
	return out.Data.ID, nil
}

// ensureAlias binds an entity to a username on the auth mount, so a direct Vault login resolves to
// the platform-managed entity (and its group policies). Idempotent: a duplicate alias is tolerated.
func (c *Client) ensureAlias(ctx context.Context, entityID, name, accessor string) error {
	return c.do(ctx, http.MethodPost, "/v1/identity/entity-alias", map[string]any{
		"name":           name,
		"canonical_id":   entityID,
		"mount_accessor": accessor,
	}, nil, http.StatusBadRequest)
}

// putGroup creates or updates an internal identity group by name with policies and member entity ids.
func (c *Client) putGroup(ctx context.Context, name string, policies, memberIDs []string) error {
	if policies == nil {
		policies = []string{}
	}
	if memberIDs == nil {
		memberIDs = []string{}
	}
	return c.do(ctx, http.MethodPost, "/v1/identity/group", map[string]any{
		"name":              name,
		"type":              "internal",
		"policies":          policies,
		"member_entity_ids": memberIDs,
	}, nil)
}
